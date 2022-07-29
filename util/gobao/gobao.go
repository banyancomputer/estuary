package gobao

// NOTE: There should be NO space between the comments and the `import "C"` line.

/*
#cgo LDFLAGS: -L./lib -lobao
#include "./lib/obao.h"
*/
import "C"

func process_file() {
//     C.init_stuff()
	C.process_file(C.CString("ethereum.pdf"))
}
