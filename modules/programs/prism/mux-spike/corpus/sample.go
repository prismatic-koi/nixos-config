// sample.go — corpus fixture used by the nvim corpus entry. Picked to
// exercise syntax-highlight rendering, comments, strings, and a tab so
// 'list' invisibles surface.
package sample

import (
	"fmt"
	"strings"
)

// Greet returns a friendly greeting for name.
func Greet(name string) string {
	if name == "" {
		name = "world"
	}
	return fmt.Sprintf("kia ora, %s!", strings.ToLower(name))
}

func main() {
	for _, who := range []string{"Ben", "", "MUX"} {
		fmt.Println(Greet(who))
	}
}
