// Command hello is the service of the root filesystem: it prints a message and then
// waits until it is told to stop.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	fmt.Println("Hello from the rules_img disk image!")
	fmt.Println("Booted from a unified kernel image on a GPT disk: erofs root, ext4 for /var and /home.")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	fmt.Println("Stopping.")
}
