package plugin_mp4

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/storage"
	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
)

type redirectStorage struct{ key string }

func (s *redirectStorage) CreateFile(context.Context, string) (storage.File, error) {
	return nil, nil
}
func (s *redirectStorage) OpenFile(context.Context, string) (storage.File, error) {
	return nil, nil
}
func (s *redirectStorage) Delete(context.Context, string) error { return nil }
func (s *redirectStorage) Exists(context.Context, string) (bool, error) {
	return true, nil
}
func (s *redirectStorage) GetSize(context.Context, string) (int64, error) { return 1, nil }
func (s *redirectStorage) GetURL(context.Context, string) (string, error) {
	return "https://object.invalid/record.mp4", nil
}
func (s *redirectStorage) List(context.Context, string) ([]storage.FileInfo, error) {
	return nil, nil
}
func (s *redirectStorage) Close() error   { return nil }
func (s *redirectStorage) GetKey() string { return s.key }

func TestDownloadSingleFileResolvesRecordStorageInsteadOfActiveStorage(t *testing.T) {
	const objectType = "mp4-object-test"
	objectBackend := &redirectStorage{key: objectType}
	var resolvedType string
	resolver := func(storageType string) (storage.Storage, error) {
		resolvedType = storageType
		return objectBackend, nil
	}
	plugin := &MP4Plugin{}
	stream := &m7s.RecordStream{StorageType: objectType, FilePath: "record.mp4"}
	request := httptest.NewRequest(http.MethodGet, "/mp4/download/live/test?id=1", nil)
	response := httptest.NewRecorder()

	plugin.downloadSingleFileWithResolver(resolver, *stream, 0, response, request)

	if resolvedType != objectType {
		t.Fatalf("resolved type=%q, want %q", resolvedType, objectType)
	}
	if response.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Location") != "https://object.invalid/record.mp4" {
		t.Fatalf("location=%q", response.Header().Get("Location"))
	}
}

func TestDownloadSingleFileReturnsGenericServiceUnavailableWhenResolverFails(t *testing.T) {
	const (
		objectType = "retired-object-storage"
		secret     = "endpoint=https://admin:password@storage.invalid"
	)
	plugin := &MP4Plugin{}
	stream := m7s.RecordStream{StorageType: objectType, FilePath: "records/a.mp4"}
	request := httptest.NewRequest(http.MethodGet, "/mp4/download/live/test?id=1", nil)
	response := httptest.NewRecorder()

	plugin.downloadSingleFileWithResolver(func(storageType string) (storage.Storage, error) {
		if storageType != objectType {
			t.Fatalf("resolved type=%q, want %q", storageType, objectType)
		}
		return nil, errors.New(secret)
	}, stream, 0, response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); body != "record storage is temporarily unavailable\n" {
		t.Fatalf("body=%q, want generic service unavailable message", body)
	} else if strings.Contains(body, secret) || strings.Contains(body, "password") {
		t.Fatalf("body leaks resolver details: %q", body)
	}
}

type closeTrackingFile struct {
	storage.File
	closeCalls int
}

func (f *closeTrackingFile) Close() error {
	f.closeCalls++
	return f.File.Close()
}

type rangeTestStorage struct {
	key           string
	files         map[string]*closeTrackingFile
	openErrors    map[string]error
	openKeys      []string
	levelOpenKeys []string
	levels        []int
	getURLCalls   int
}

func (s *rangeTestStorage) CreateFile(context.Context, string) (storage.File, error) {
	return nil, nil
}

func (s *rangeTestStorage) OpenFile(_ context.Context, key string) (storage.File, error) {
	s.openKeys = append(s.openKeys, key)
	if err := s.openErrors[key]; err != nil {
		return nil, err
	}
	file, ok := s.files[key]
	if !ok {
		return nil, errors.New("test file not configured")
	}
	return file, nil
}

func (s *rangeTestStorage) OpenFileFromStorageLevel(_ context.Context, key string, level int) (storage.File, error) {
	s.levelOpenKeys = append(s.levelOpenKeys, key)
	s.levels = append(s.levels, level)
	if err := s.openErrors[key]; err != nil {
		return nil, err
	}
	file, ok := s.files[key]
	if !ok {
		return nil, errors.New("test file not configured")
	}
	return file, nil
}

func (s *rangeTestStorage) Delete(context.Context, string) error { return nil }
func (s *rangeTestStorage) Exists(context.Context, string) (bool, error) {
	return true, nil
}
func (s *rangeTestStorage) GetSize(context.Context, string) (int64, error) { return 0, nil }
func (s *rangeTestStorage) GetURL(context.Context, string) (string, error) {
	s.getURLCalls++
	return "https://object.invalid/presigned", nil
}
func (s *rangeTestStorage) List(context.Context, string) ([]storage.FileInfo, error) {
	return nil, nil
}
func (s *rangeTestStorage) Close() error   { return nil }
func (s *rangeTestStorage) GetKey() string { return s.key }

func newCloseTrackingFile(t *testing.T, data []byte) *closeTrackingFile {
	t.Helper()
	path := t.TempDir() + "/record.mp4"
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return &closeTrackingFile{File: &storage.LocalFile{File: file}}
}

func minimalMP4Data(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	muxer := mp4pkg.NewMuxer(0)
	if err := muxer.WriteInitSegment(&output); err != nil {
		t.Fatalf("write init segment: %v", err)
	}
	if err := muxer.WriteMoov(&output); err != nil {
		t.Fatalf("write moov: %v", err)
	}
	return output.Bytes()
}

func testRangeResolver(backends map[string]storage.Storage) recordStorageResolver {
	return func(storageType string) (storage.Storage, error) {
		backend, ok := backends[storageType]
		if !ok {
			return nil, errors.New("storage configuration contains secret-value")
		}
		return backend, nil
	}
}

func testRangeStream(storageType, filePath string, level int, start time.Time) m7s.RecordStream {
	return m7s.RecordStream{
		StorageType:  storageType,
		StorageLevel: level,
		FilePath:     filePath,
		StartTime:    start,
		EndTime:      start.Add(time.Second),
	}
}

func TestDownloadRangeClosesMixedStorageFilesExactlyOnceOnSuccess(t *testing.T) {
	data := minimalMP4Data(t)
	localFile := newCloseTrackingFile(t, data)
	objectFile := newCloseTrackingFile(t, data)
	localBackend := &rangeTestStorage{
		key:   string(storage.StorageTypeLocal),
		files: map[string]*closeTrackingFile{"backup/local.mp4": localFile},
	}
	objectBackend := &rangeTestStorage{
		key:   "archive-a",
		files: map[string]*closeTrackingFile{"records/object.mp4": objectFile},
	}
	start := time.Now().Add(-time.Minute)
	streams := []m7s.RecordStream{
		testRangeStream(string(storage.StorageTypeLocal), "backup/local.mp4", 2, start),
		testRangeStream("archive-a", "records/object.mp4", 0, start.Add(time.Second)),
	}
	request := httptest.NewRequest(http.MethodGet, "/mp4/download/live/test", nil)
	response := httptest.NewRecorder()

	(&MP4Plugin{}).downloadRangeWithResolver(
		testRangeResolver(map[string]storage.Storage{
			string(storage.StorageTypeLocal): localBackend,
			"archive-a":                      objectBackend,
		}),
		streams,
		time.Time{},
		time.Time{},
		0,
		response,
		request,
	)

	if localFile.closeCalls != 1 || objectFile.closeCalls != 1 {
		t.Fatalf("close calls local=%d object=%d, want 1 each", localFile.closeCalls, objectFile.closeCalls)
	}
	if len(localBackend.levelOpenKeys) != 1 || localBackend.levelOpenKeys[0] != "backup/local.mp4" || localBackend.levels[0] != 2 {
		t.Fatalf("local opens keys=%v levels=%v", localBackend.levelOpenKeys, localBackend.levels)
	}
	if len(objectBackend.openKeys) != 1 || objectBackend.openKeys[0] != "records/object.mp4" {
		t.Fatalf("object OpenFile keys=%v", objectBackend.openKeys)
	}
	if objectBackend.getURLCalls != 0 {
		t.Fatalf("object GetURL calls=%d, want 0", objectBackend.getURLCalls)
	}
}

func TestDownloadRangeClosesFileOnDemuxFailure(t *testing.T) {
	file := newCloseTrackingFile(t, []byte("not-an-mp4"))
	backend := &rangeTestStorage{key: "archive-a", files: map[string]*closeTrackingFile{"broken.mp4": file}}
	request := httptest.NewRequest(http.MethodGet, "/mp4/download/live/test", nil)
	response := httptest.NewRecorder()

	(&MP4Plugin{}).downloadRangeWithResolver(
		testRangeResolver(map[string]storage.Storage{"archive-a": backend}),
		[]m7s.RecordStream{testRangeStream("archive-a", "broken.mp4", 0, time.Now())},
		time.Time{},
		time.Time{},
		0,
		response,
		request,
	)

	if file.closeCalls != 1 {
		t.Fatalf("close calls=%d, want 1 after demux failure", file.closeCalls)
	}
}

func TestDownloadRangeClosesPreviouslyOpenedFilesOnLaterStorageFailures(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		secondType string
		openErrors map[string]error
		backends   func(*rangeTestStorage) map[string]storage.Storage
	}{
		{
			name:       "resolver failure",
			secondType: "missing-archive",
			backends: func(first *rangeTestStorage) map[string]storage.Storage {
				return map[string]storage.Storage{"archive-a": first}
			},
		},
		{
			name:       "open failure",
			secondType: "archive-a",
			openErrors: map[string]error{"second.mp4": errors.New("open failed")},
			backends: func(first *rangeTestStorage) map[string]storage.Storage {
				return map[string]storage.Storage{"archive-a": first}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			firstFile := newCloseTrackingFile(t, minimalMP4Data(t))
			backend := &rangeTestStorage{
				key:        "archive-a",
				files:      map[string]*closeTrackingFile{"first.mp4": firstFile},
				openErrors: testCase.openErrors,
			}
			start := time.Now().Add(-time.Minute)
			request := httptest.NewRequest(http.MethodGet, "/mp4/download/live/test", nil)
			response := httptest.NewRecorder()

			(&MP4Plugin{}).downloadRangeWithResolver(
				testRangeResolver(testCase.backends(backend)),
				[]m7s.RecordStream{
					testRangeStream("archive-a", "first.mp4", 0, start),
					testRangeStream(testCase.secondType, "second.mp4", 0, start.Add(time.Second)),
				},
				time.Time{},
				time.Time{},
				0,
				response,
				request,
			)

			if firstFile.closeCalls != 1 {
				t.Fatalf("first close calls=%d, want 1", firstFile.closeCalls)
			}
		})
	}
}

func TestDownloadRangeClosesFileAfterSeekContinue(t *testing.T) {
	file := newCloseTrackingFile(t, minimalMP4Data(t))
	backend := &rangeTestStorage{key: "archive-a", files: map[string]*closeTrackingFile{"seek.mp4": file}}
	streamStart := time.Now().Add(-time.Minute)
	request := httptest.NewRequest(http.MethodGet, "/mp4/download/live/test", nil)
	response := httptest.NewRecorder()

	(&MP4Plugin{}).downloadRangeWithResolver(
		testRangeResolver(map[string]storage.Storage{"archive-a": backend}),
		[]m7s.RecordStream{testRangeStream("archive-a", "seek.mp4", 0, streamStart)},
		streamStart.Add(500*time.Millisecond),
		time.Time{},
		0,
		response,
		request,
	)

	if file.closeCalls != 1 {
		t.Fatalf("close calls=%d, want 1 after seek continue", file.closeCalls)
	}
}

type failingResponseWriter struct {
	header http.Header
}

func (w *failingResponseWriter) Header() http.Header { return w.header }
func (w *failingResponseWriter) WriteHeader(int)     {}
func (w *failingResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("response write failed")
}

func TestDownloadRangeClosesFileOnResponseWriteFailure(t *testing.T) {
	file := newCloseTrackingFile(t, minimalMP4Data(t))
	backend := &rangeTestStorage{key: "archive-a", files: map[string]*closeTrackingFile{"write.mp4": file}}
	request := httptest.NewRequest(http.MethodGet, "/mp4/download/live/test", nil)
	response := &failingResponseWriter{header: make(http.Header)}

	(&MP4Plugin{}).downloadRangeWithResolver(
		testRangeResolver(map[string]storage.Storage{"archive-a": backend}),
		[]m7s.RecordStream{testRangeStream("archive-a", "write.mp4", 0, time.Now())},
		time.Time{},
		time.Time{},
		0,
		response,
		request,
	)

	if file.closeCalls != 1 {
		t.Fatalf("close calls=%d, want 1 after response write failure", file.closeCalls)
	}
}
