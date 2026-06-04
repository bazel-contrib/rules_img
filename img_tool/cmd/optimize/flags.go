package optimize

import (
	"fmt"
	"os"
	"strings"
)

type fileList []string

func (l *fileList) String() string {
	return strings.Join(*l, ", ")
}

func (l *fileList) Set(value string) error {
	if _, err := os.Stat(value); err != nil {
		return fmt.Errorf("file %s does not exist: %w", value, err)
	}
	*l = append(*l, value)
	return nil
}
