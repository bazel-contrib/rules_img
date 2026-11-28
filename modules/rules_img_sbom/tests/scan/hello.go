package main

import (
	"fmt"

	"github.com/google/uuid"
)

func main() {
	fmt.Println(uuid.NewSHA1(uuid.NameSpaceURL, []byte("hello")))
}
