//go:build !pscal_embed

package main

import "os"

func pscalLookupEnv(name string) string {
	return os.Getenv(name)
}
