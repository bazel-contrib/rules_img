package deploymetadata

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

// doubleIndexedStringListFlag handles flags in the format --flag=idx1,idx2=val1,val2,...
type doubleIndexedStringListFlag struct {
	values map[int]map[int][]string
}

func newDoubleIndexedStringListFlag() *doubleIndexedStringListFlag {
	return &doubleIndexedStringListFlag{
		values: make(map[int]map[int][]string),
	}
}

func (f *doubleIndexedStringListFlag) String() string {
	var parts []string
	for idx1, inner := range f.values {
		for idx2, vals := range inner {
			parts = append(parts, fmt.Sprintf("%d,%d=%s", idx1, idx2, strings.Join(vals, ",")))
		}
	}
	return strings.Join(parts, ", ")
}

func (f *doubleIndexedStringListFlag) Set(value string) error {
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
		f.values[idx1] = make(map[int][]string)
	}

	vals := strings.Split(parts[1], ",")
	if len(vals) == 1 && vals[0] == "" {
		vals = nil
	}

	if f.values[idx1][idx2] == nil {
		f.values[idx1][idx2] = vals
	} else {
		f.values[idx1][idx2] = append(f.values[idx1][idx2], vals...)
	}
	return nil
}
