// Command agent-bus-web serves a live view of the agent bus in the browser.
package main

import (
	"os"

	bus "agent-bus/src"
)

func main() {
	bus.Web(os.Args[1:])
}
