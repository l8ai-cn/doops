package main

import (
	"fmt"
	"io"
	"os"
)

func resolveWriteContent(contentFlag, fileFlag string, positional []string, stdin *os.File) (string, error) {
	sourceCount := 0
	if contentFlag != "" {
		sourceCount++
	}
	if fileFlag != "" {
		sourceCount++
	}
	if len(positional) > 0 {
		sourceCount++
	}
	if sourceCount > 1 {
		return "", fmt.Errorf("write content source is ambiguous; use only one of --content, --file, stdin, or positional content")
	}

	switch {
	case contentFlag != "":
		return contentFlag, nil
	case fileFlag != "":
		if fileFlag == "-" {
			data, err := io.ReadAll(stdin)
			if err != nil {
				return "", fmt.Errorf("read stdin failed: %w", err)
			}
			return string(data), nil
		}
		data, err := os.ReadFile(fileFlag)
		if err != nil {
			return "", fmt.Errorf("read %s failed: %w", fileFlag, err)
		}
		return string(data), nil
	case len(positional) > 0:
		return positional[0], nil
	default:
		stat, err := stdin.Stat()
		if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
			data, err := io.ReadAll(stdin)
			if err != nil {
				return "", fmt.Errorf("read stdin failed: %w", err)
			}
			return string(data), nil
		}
		return "", fmt.Errorf("content required for write; use --content, --file, stdin, or positional content")
	}
}
