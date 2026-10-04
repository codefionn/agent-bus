// Command agent-bus is a message bus between the agent sessions on one machine.
package main

import (
	"os"

	bus "agent-bus/src"
)

func main() {
	bus.Main(os.Args[1:])
}
