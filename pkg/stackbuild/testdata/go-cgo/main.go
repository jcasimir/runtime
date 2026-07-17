package main

// A minimal cgo program: the `import "C"` plus a C snippet forces the build
// onto the cgo path (CGO_ENABLED=1, glibc runtime) without pulling any external
// module, keeping the build hermetic. Uses only the C stdlib.

/*
#include <stdio.h>

static void greet(void) {
	printf("hello from cgo\n");
}
*/
import "C"

func main() {
	C.greet()
}
