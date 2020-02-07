// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package logsearch

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pingcap/kvproto/pkg/diagnosticspb"
	"github.com/pingcap/sysutil"
	"google.golang.org/grpc"
)

func (c *Component) address() string {
	port := c.Port
	if c.ServerType == "tidb" {
		port = c.StatusPort
	}
	return fmt.Sprintf("%s:%s", c.IP, port)
}

func (c *Component) zipFilename() string {
	return fmt.Sprintf("%s-%s.zip", c.IP, c.Port)
}

func (c *Component) logFilename() string {
	return fmt.Sprintf("%s.log", c.ServerType)
}

type Task struct {
	*TaskModel
	mu     sync.Mutex
	cancel context.CancelFunc
	doneCh chan struct{}
}

func NewTask(component *Component, taskGroupID string, req *SearchLogRequest) *Task {
	return &Task{
		TaskModel: &TaskModel{
			Component:   component,
			Request:     req,
			TaskGroupID: taskGroupID,
			TaskID:      uuid.New().String(),
			CreateTime:  time.Now().Unix(),
		},
		mu: sync.Mutex{},
	}
}

func ToTask(t *TaskModel) *Task {
	return &Task{
		TaskModel: t,
		mu:        sync.Mutex{},
	}
}

func (t *Task) Abort() error {
	if t.cancel != nil {
		t.doneCh = make(chan struct{})
		t.cancel()
		// ensure the task has been aborted
		<-t.doneCh
		return nil
	}
	return fmt.Errorf("task [%s] is not running", t.TaskID)
}

func (t *Task) done() {
	if t.doneCh != nil {
		t.doneCh <- struct{}{}
	}
}

func (t *Task) close() {
	defer t.done()
	if t.Error != "" {
		fmt.Printf("task [%s] stoped, err=%s", t.TaskID, t.Error)
		t.clean()
		t.StopTime = time.Now().Unix()
		t.mu.Lock()
		t.State = StateCanceled
		dbClient.updateTask(t.TaskModel)
		t.mu.Unlock()
		return
	}
	t.StopTime = time.Now().Unix()
	t.mu.Lock()
	t.State = StateFinished
	dbClient.updateTask(t.TaskModel)
	t.mu.Unlock()
}

func (t *Task) clean() error {
	var err error
	if t.SavedPath != "" {
		err = os.RemoveAll(t.SavedPath)
		if err != nil {
			return err
		}
	}
	dbClient.cleanPreview(t.TaskID)
	return err
}

const PreviewLogLinesLimit = 500

func (t *Task) run() {
	defer t.close()
	var ctx context.Context
	ctx, t.cancel = context.WithCancel(context.Background())
	opt := grpc.WithInsecure()

	conn, err := grpc.Dial(t.Component.address(), opt)
	if err != nil {
		t.Error = err.Error()
		return
	}
	defer conn.Close()
	cli := diagnosticspb.NewDiagnosticsClient(conn)
	stream, err := cli.SearchLog(ctx, (*diagnosticspb.SearchLogRequest)(t.Request))
	if err != nil {
		t.Error = err.Error()
		return
	}

	dir := path.Join(logsSavePath, t.TaskGroupID)
	err = os.MkdirAll(dir, 0777)
	if err != nil {
		t.Error = err.Error()
		return
	}
	savedPath := path.Join(dir, t.Component.zipFilename())
	f, err := os.Create(savedPath)
	if err != nil {
		t.Error = err.Error()
		return
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()
	writer, err := zw.Create(t.Component.logFilename())
	if err != nil {
		t.Error = err.Error()
		return
	}
	t.SavedPath = savedPath
	if err != nil {
		t.Error = err.Error()
		return
	}

	t.StartTime = time.Now().Unix()
	t.mu.Lock()
	t.State = StateRunning
	dbClient.deleteTask(t.TaskModel)
	dbClient.createTask(t.TaskModel)
	t.mu.Unlock()
	if err != nil {
		t.Error = err.Error()
		return
	}

	previewLogLinesCount := 0
	for {
		res, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				t.Error = err.Error()
			}
			return
		}
		for _, msg := range res.Messages {
			line := toLine(msg)
			// TODO: use unsafe here: string -> []byte
			_, err := writer.Write([]byte(line))
			if err != nil {
				t.Error = err.Error()
				return
			}
			if previewLogLinesCount < PreviewLogLinesLimit {
				dbClient.newPreview(t.TaskID, msg)
				previewLogLinesCount++
			}
		}
		err = zw.Flush()
		if err != nil {
			t.Error = err.Error()
			return
		}
	}
}

func toLine(msg *diagnosticspb.LogMessage) string {
	timeStr := time.Unix(0, msg.Time*int64(time.Millisecond)).Format(sysutil.TimeStampLayout)
	return fmt.Sprintf("[%s] [%s] %s\n", timeStr, diagnosticspb.LogLevel_name[int32(msg.Level)], msg.Message)
}
