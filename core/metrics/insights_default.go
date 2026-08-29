//go:build !linux && !windows && !darwin

package metrics

import "errors"

// getOSVersion 在不支持的平台上返回空值。
func getOSVersion() (string, string) { return "", "" }

// getFilesystemType 在不支持的平台上不做探测。
func getFilesystemType(_ string) (string, error) { return "", errors.New("not implemented") }
