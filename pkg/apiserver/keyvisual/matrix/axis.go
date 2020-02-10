// Copyright 2019 PingCAP, Inc.
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

package matrix

// Axis stores consecutive buckets. Each bucket has StartKey, EndKey, and some statistics. The EndKey of each bucket is
// the StartKey of its next bucket. The actual data structure is stored in columns. Therefore satisfies:
// len(Keys) == len(ValuesList[i]) + 1. In particular, ValuesList[0] is the base column.
type Axis struct {
	Keys       []string
	ValuesList [][]uint64
}

// CreateAxis checks the given parameters and uses them to build the Axis.
func CreateAxis(keys []string, valuesList [][]uint64) Axis {
	keysLen := len(keys)
	if keysLen <= 1 {
		panic("Keys length must be greater than 1")
	}
	if len(valuesList) == 0 {
		panic("ValuesList length must be greater than 0")
	}
	for _, values := range valuesList {
		if keysLen != len(values)+1 {
			panic("Keys length must be equal to Values length + 1")
		}
	}
	return Axis{
		Keys:       keys,
		ValuesList: valuesList,
	}
}

// CreateEmptyAxis constructs a minimal empty Axis with the given parameters.
func CreateEmptyAxis(startKey, endKey string, valuesListLen int) Axis {
	keys := []string{startKey, endKey}
	values := []uint64{0}
	valuesList := make([][]uint64, valuesListLen)
	for i := range valuesList {
		valuesList[i] = values
	}
	return CreateAxis(keys, valuesList)
}

// Shrink reduces all statistical values.
func (axis *Axis) Shrink(ratio uint64) {
	for _, values := range axis.ValuesList {
		for i := range values {
			values[i] /= ratio
		}
	}
}

// Range returns a sub Axis with specified range.
func (axis *Axis) Range(startKey string, endKey string) Axis {
	start, end, ok := KeysRange(axis.Keys, startKey, endKey)
	if !ok {
		return CreateEmptyAxis(startKey, endKey, len(axis.ValuesList))
	}
	keys := axis.Keys[start:end]
	valuesList := make([][]uint64, len(axis.ValuesList))
	for i := range valuesList {
		valuesList[i] = axis.ValuesList[i][start : end-1]
	}
	return CreateAxis(keys, valuesList)
}

// Focus uses the base column as the chunk for the Focus operation to obtain the partitioning scheme, and uses this to
// reduce other columns.
func (axis *Axis) Focus(strategy Strategy, threshold uint64, ratio int, target int) Axis {
	if target >= len(axis.Keys)-1 {
		return *axis
	}

	baseChunk := createChunk(axis.Keys, axis.ValuesList[0])
	newChunk := baseChunk.Focus(strategy, threshold, ratio, target)
	valuesListLen := len(axis.ValuesList)
	newValuesList := make([][]uint64, valuesListLen)
	newValuesList[0] = newChunk.Values
	for i := 1; i < valuesListLen; i++ {
		baseChunk.SetValues(axis.ValuesList[i])
		newValuesList[i] = baseChunk.Reduce(newChunk.Keys).Values
	}
	return CreateAxis(newChunk.Keys, newValuesList)
}

// Divide uses the base column as the chunk for the Divide operation to obtain the partitioning scheme, and uses this to
// reduce other columns.
func (axis *Axis) Divide(strategy Strategy, target int) Axis {
	if target >= len(axis.Keys)-1 {
		return *axis
	}

	baseChunk := createChunk(axis.Keys, axis.ValuesList[0])
	newChunk := baseChunk.Divide(strategy, target)
	valuesListLen := len(axis.ValuesList)
	newValuesList := make([][]uint64, valuesListLen)
	newValuesList[0] = newChunk.Values
	for i := 1; i < valuesListLen; i++ {
		baseChunk.SetValues(axis.ValuesList[i])
		newValuesList[i] = baseChunk.Reduce(newChunk.Keys).Values
	}
	return CreateAxis(newChunk.Keys, newValuesList)
}

type chunk struct {
	// Keys and ValuesList[i] from Axis
	Keys   []string
	Values []uint64
}

func createChunk(keys []string, values []uint64) chunk {
	keysLen := len(keys)
	if keysLen <= 1 {
		panic("Keys length must be greater than 1")
	}
	if keysLen != len(values)+1 {
		panic("Keys length must be equal to Values length + 1")
	}
	return chunk{
		Keys:   keys,
		Values: values,
	}
}

func createZeroChunk(keys []string) chunk {
	keysLen := len(keys)
	if keysLen <= 1 {
		panic("Keys length must be greater than 1")
	}
	return createChunk(keys, make([]uint64, keysLen-1))
}

func (c *chunk) SetValues(values []uint64) {
	if len(values)+1 != len(c.Keys) {
		panic("Keys length must be equal to Values length + 1")
	}
	c.Values = values
}

func (c *chunk) SetZeroValues() {
	newValues := make([]uint64, len(c.Values))
	c.SetValues(newValues)
}

// Set all values to 0
func (c *chunk) Clear() {
	MemsetUint64(c.Values, 0)
}

// Calculation

// Reduce generates new chunks based on the more sparse newKeys
func (c *chunk) Reduce(newKeys []string) chunk {
	keys := c.Keys
	CheckReduceOf(keys, newKeys)

	newValues := make([]uint64, len(newKeys)-1)

	if len(keys) == len(newKeys) {
		copy(newValues, c.Values)
		return createChunk(newKeys, newValues)
	}

	endKeys := newKeys[1:]
	j := 0
	for i, value := range c.Values {
		if equal(keys[i], endKeys[j]) {
			j++
		}
		newValues[j] += value
	}
	return createChunk(newKeys, newValues)
}

// GetFocusRows estimates the number of rows generated by executing a Focus with a specified threshold
func (c *chunk) GetFocusRows(threshold uint64) (count int) {
	start := 0
	var bucketSum uint64 = 0
	generateBucket := func(end int) {
		if end > start {
			count++
			start = end
			bucketSum = 0
		}
	}

	for i, value := range c.Values {
		if value >= threshold || bucketSum >= threshold {
			generateBucket(i)
		}
		bucketSum += value
	}
	generateBucket(len(c.Values))

	return
}

// Given a `threshold`, merge the rows with less traffic,
// and merge the most `ratio` rows at a time.
// `target` is the estimated final number of rows.
func (c *chunk) Focus(strategy Strategy, threshold uint64, ratio int, target int) chunk {
	newKeys := make([]string, 0, target)
	newValues := make([]uint64, 0, target)
	newKeys = append(newKeys, c.Keys[0])

	start := 0
	var bucketSum uint64 = 0
	generateBucket := func(end int) {
		if end > start {
			newKeys = append(newKeys, c.Keys[end])
			newValues = append(newValues, bucketSum)
			start = end
			bucketSum = 0
		}
	}

	for i, value := range c.Values {
		if value >= threshold ||
			bucketSum >= threshold ||
			i-start >= ratio ||
			strategy.CrossBorder(c.Keys[start], c.Keys[i]) {
			generateBucket(i)
		}
		bucketSum += value
	}
	generateBucket(len(c.Values))

	return createChunk(newKeys, newValues)
}

// Divide uses binary search to find a suitable threshold, which can reduce the number of buckets of Axis to near the target.
func (c *chunk) Divide(strategy Strategy, target int) chunk {
	if target >= len(c.Values) {
		return *c
	}
	// get upperThreshold
	var upperThreshold uint64 = 1
	for _, value := range c.Values {
		upperThreshold += value
	}
	// search threshold
	var lowerThreshold uint64 = 1
	targetFocusRows := target * 2 / 3 // TODO: This var can be adjusted
	for lowerThreshold < upperThreshold {
		mid := (lowerThreshold + upperThreshold) >> 1
		if c.GetFocusRows(mid) > targetFocusRows {
			lowerThreshold = mid + 1
		} else {
			upperThreshold = mid
		}
	}

	threshold := lowerThreshold
	focusRows := c.GetFocusRows(threshold)
	ratio := len(c.Values)/(target-focusRows) + 1
	return c.Focus(strategy, threshold, ratio, target)
}
