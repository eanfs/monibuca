package mp4

import (
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"m7s.live/v5/plugin/mp4/pkg/box"
)

// TestDemuxerBoxTree tests the Demuxer by reading a test MP4 file and printing the box tree structure.
func TestDemuxerBoxTree(t *testing.T) {
	// Open the test mp4 file. It is assumed to be located in 'testdata/test_regular.mp4'.
	f, err := os.Open("/Users/dexter/project/v5/monibuca/example/default/dump/test_regular.mp4")
	if err != nil {
		t.Fatalf("failed to open test mp4 file: %v", err)
	}
	defer f.Close()

	// Create a new Demuxer with the file reader
	d := NewDemuxer(f)

	// Call Demux to process the file; we don't use the result directly here.
	err = d.Demux()
	if err != nil {
		t.Fatalf("demuxing failed: %v", err)
	}

	// Reset the file pointer to the beginning to re-read boxes for tree display
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("failed to seek to beginning: %v", err)
	}

	fmt.Println("MP4 Box Tree:")
	// Read and print each top-level box
	for {
		b, err := box.ReadFrom(f)
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("failed to read box: %v", err)
		}
		printBox(b, 0)
	}
}

// printBox prints a box's type and size, and recursively prints its children if available.
func printBox(b interface{}, indent int) {
	ind := strings.Repeat("  ", indent)
	// Determine the box type name using reflection
	typeOfBox := reflect.TypeOf(b)
	if typeOfBox.Kind() == reflect.Ptr {
		typeOfBox = typeOfBox.Elem()
	}
	// Try to get the size from a method Size() int
	var size int
	if s, ok := b.(interface{ Size() int }); ok {
		size = s.Size()
	}
	fmt.Printf("%s%s (size: %d)\n", ind, typeOfBox.Name(), size)

	// If the box is a container and has child boxes, print them recursively.
	if container, ok := b.(interface{ ChildrenBoxes() []interface{} }); ok {
		for _, child := range container.ChildrenBoxes() {
			printBox(child, indent+1)
		}
	} else if container, ok := b.(interface{ ChildrenBoxes() []any }); ok {
		for _, child := range container.ChildrenBoxes() {
			printBox(child, indent+1)
		}
	}
}
