// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Command agent-capacity prints usage for the service entry point.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stdout, "Usage: agent-capacity")
}
