package indexfromocilayout

import (
	"fmt"
	"strconv"
	"strings"
)

// indexedStringFlag handles flags in the format --flag=index=value
type indexedStringFlag struct {
	values map[int]string
}

func newIndexedStringFlag() *indexedStringFlag {
	return &indexedStringFlag{
		values: make(map[int]string),
	}
}

func (f *indexedStringFlag) String() string {
	var parts []string
	for idx, val := range f.values {
		parts = append(parts, fmt.Sprintf("%d=%s", idx, val))
	}
	return strings.Join(parts, ", ")
}

func (f *indexedStringFlag) Set(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid format, expected index=value: %s", value)
	}

	idx, err := strconv.Atoi(parts[0])
	if err != nil {
		return fmt.Errorf("invalid index: %s", parts[0])
	}

	f.values[idx] = parts[1]
	return nil
}

func (f *indexedStringFlag) Get(idx int) (string, bool) {
	val, ok := f.values[idx]
	return val, ok
}

// doubleIndexedStringFlag handles flags in the format --flag=idx1,idx2=value
type doubleIndexedStringFlag struct {
	values map[int]map[int]string
}

func newDoubleIndexedStringFlag() *doubleIndexedStringFlag {
	return &doubleIndexedStringFlag{
		values: make(map[int]map[int]string),
	}
}

func (f *doubleIndexedStringFlag) String() string {
	var parts []string
	for idx1, inner := range f.values {
		for idx2, val := range inner {
			parts = append(parts, fmt.Sprintf("%d,%d=%s", idx1, idx2, val))
		}
	}
	return strings.Join(parts, ", ")
}

func (f *doubleIndexedStringFlag) Set(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid format, expected idx1,idx2=value: %s", value)
	}

	indices := strings.Split(parts[0], ",")
	if len(indices) != 2 {
		return fmt.Errorf("invalid format, expected two indices: %s", parts[0])
	}

	idx1, err := strconv.Atoi(indices[0])
	if err != nil {
		return fmt.Errorf("invalid first index: %s", indices[0])
	}

	idx2, err := strconv.Atoi(indices[1])
	if err != nil {
		return fmt.Errorf("invalid second index: %s", indices[1])
	}

	if f.values[idx1] == nil {
		f.values[idx1] = make(map[int]string)
	}
	f.values[idx1][idx2] = parts[1]
	return nil
}

func (f *doubleIndexedStringFlag) Get(idx1, idx2 int) (string, bool) {
	if inner, ok := f.values[idx1]; ok {
		val, ok := inner[idx2]
		return val, ok
	}
	return "", false
}

func (f *doubleIndexedStringFlag) GetManifestCount() int {
	return len(f.values)
}

func (f *doubleIndexedStringFlag) GetLayerCount(manifestIdx int) int {
	if inner, ok := f.values[manifestIdx]; ok {
		return len(inner)
	}
	return 0
}
