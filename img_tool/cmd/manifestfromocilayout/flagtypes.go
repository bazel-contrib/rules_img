package manifestfromocilayout

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
