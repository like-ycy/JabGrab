//go:build !windows

package downloader

func decodeOutput(raw []byte) (string, error) {
	return string(raw), nil
}
