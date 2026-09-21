package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func downloadFile(path, url string) error {
	// Bounded: a stalled server must not wedge a command worker forever.
	// 100MB cap at ~1.4Mbps needs ~10min; checksum+signature verify after.
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download %s: unexpected status %s", url, resp.Status)
	}

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
