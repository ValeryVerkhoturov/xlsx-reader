package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/yurij-lyubskij/xlsx-reader/zipstream"
)

func unpack(path string) error {
	r, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer r.Close()

	fmt.Printf("=== [archive/zip]  %s (%d entries) ===\n", path, len(r.File))

	for _, f := range r.File {
		fmt.Printf("\n--- %s (%d bytes) ---\n", f.Name, f.UncompressedSize64)

		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("open entry %s: %w", f.Name, err)
		}

		if _, err := io.Copy(os.Stdout, rc); err != nil {
			rc.Close()
			return fmt.Errorf("read entry %s: %w", f.Name, err)
		}
		rc.Close()
		fmt.Println()
	}
	fmt.Println()
	return nil
}

func unpackStream(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	fmt.Printf("=== [zipstream]    %s ===\n", path)

	w := zipstream.New(f)
	for {
		name, entry, err := w.Next()
		if err != nil {
			return fmt.Errorf("next entry in %s: %w", path, err)
		}
		if entry == nil {
			break // central-directory signature or true EOF — no more entries
		}

		var buf bytes.Buffer
		if _, err := io.Copy(&buf, entry); err != nil {
			return fmt.Errorf("read entry %s: %w", name, err)
		}

		if err := entry.Finish(); err != nil {
			return fmt.Errorf("finish entry %s: %w", name, err)
		}

		fmt.Printf("\n--- %s (%d bytes) ---\n", name, buf.Len())
		os.Stdout.Write(buf.Bytes())
		fmt.Println()
	}
	fmt.Println()
	return nil
}

func main() {
	paths := []string{"zips/classic_32bit.zip", "zips/zip64_64bit.zip"}

	for _, p := range paths {
		if err := unpack(p); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if err := unpackStream(p); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}
}
