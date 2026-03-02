//go:build pscal_embed

package main

/*
#include <stdlib.h>
*/
import "C"

import "unsafe"

func pscalLookupEnv(name string) string {
	if name == "" {
		return ""
	}
	cname := C.CString(name)
	if cname == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(cname))
	value := C.getenv(cname)
	if value == nil {
		return ""
	}
	return C.GoString(value)
}
