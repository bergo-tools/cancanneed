//go:build windows

package main

import "errors"

func ensureSupportedPlatform() error {
	return errors.New("Windows is not supported by cancanneed")
}
