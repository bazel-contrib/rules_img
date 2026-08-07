package erofs_test

import (
	"bytes"
	"fmt"
	"io/fs"
	"log"
	"os"

	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
)

func ExampleOpen() {
	f, err := os.Open("testdata/basic-default.erofs")
	if err != nil {
		log.Fatal(err)
	}

	img, err := erofs.Open(f)
	if err != nil {
		_ = f.Close()
		log.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	_ = fs.WalkDir(img, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	})
}

func ExampleCreate() {
	var buf testBuffer
	w := erofs.Create(&buf)

	f, err := w.Create("/hello.txt")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := f.Write([]byte("hello world\n")); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}

	if err := w.Mkdir("/dir", 0o755); err != nil {
		log.Fatal(err)
	}

	if err := w.Close(); err != nil {
		log.Fatal(err)
	}

	// Read back
	img, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		log.Fatal(err)
	}

	data, err := fs.ReadFile(img, "hello.txt")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(string(data))
	// Output: hello world
}
