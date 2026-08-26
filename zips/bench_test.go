package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/yurij-lyubskij/xlsx-reader/zipstream"
)

const entryCount = 10_000

var fixtureZip string

func TestMain(m *testing.M) {
	path, err := makeManyEntriesZip(entryCount)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench setup: %v\n", err)
		os.Exit(1)
	}
	fixtureZip = path
	code := m.Run()
	os.Remove(path)
	os.Exit(code)
}

func makeManyEntriesZip(n int) (string, error) {
	f, err := os.CreateTemp("", "bench-peak-*.zip")
	if err != nil {
		return "", err
	}
	defer f.Close()

	w := zip.NewWriter(f)
	for i := range n {
		fw, err := w.CreateHeader(&zip.FileHeader{
			Name:   fmt.Sprintf("entry_%05d.txt", i),
			Method: zip.Deflate, // Deflate is self-terminating; Store+streaming is rejected by zipstream
		})
		if err != nil {
			return "", err
		}
		if _, err := fw.Write([]byte("x")); err != nil {
			return "", err
		}
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func liveHeap() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

func BenchmarkPeakHeap_StdLib(b *testing.B) {
	debug.SetGCPercent(-1) // disable background GC so HeapAlloc only grows between GC calls
	defer debug.SetGCPercent(100)
	b.ReportAllocs()

	for b.Loop() {
		b.StopTimer()
		runtime.GC()
		baseline := liveHeap()
		b.StartTimer()

		r, err := zip.OpenReader(fixtureZip)
		if err != nil {
			b.Fatal(err)
		}

		b.StopTimer()
		b.ReportMetric(float64(liveHeap()-baseline), "peak-heap-B")
		b.StartTimer()

		for _, f := range r.File {
			rc, _ := f.Open()
			io.Copy(io.Discard, rc)
			rc.Close()
		}
		r.Close()
	}
}

func BenchmarkPeakHeap_Streaming(b *testing.B) {
	debug.SetGCPercent(-1)
	defer debug.SetGCPercent(100)
	b.ReportAllocs()

	for b.Loop() {
		f, err := os.Open(fixtureZip)
		if err != nil {
			b.Fatal(err)
		}

		b.StopTimer()
		runtime.GC()
		baseline := liveHeap()
		b.StartTimer()

		w := zipstream.New(f)
		_, entry, err := w.Next()
		if err != nil {
			b.Fatal(err)
		}

		b.StopTimer()
		b.ReportMetric(float64(liveHeap()-baseline), "peak-heap-B")
		b.StartTimer()

		for entry != nil {
			io.Copy(io.Discard, entry)
			entry.Finish()
			_, entry, err = w.Next()
			if err != nil {
				b.Fatal(err)
			}
		}
		f.Close()
	}
}
