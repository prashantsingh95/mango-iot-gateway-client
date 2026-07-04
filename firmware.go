package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

func downloadFile(path, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.ContentLength > 100*1024*1024 {
		return fmt.Errorf("file too large: %d bytes", resp.ContentLength)
	}

	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, io.LimitReader(resp.Body, 100*1024*1024))
	return err
}
