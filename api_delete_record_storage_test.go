package m7s

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/gorm"
	"m7s.live/v5/pb"
	"m7s.live/v5/pkg/storage"
)

type deleteRecordStorage struct {
	key         string
	deleteCalls []string
	deleteErr   error
}

func (s *deleteRecordStorage) CreateFile(context.Context, string) (storage.File, error) {
	return nil, nil
}
func (s *deleteRecordStorage) OpenFile(context.Context, string) (storage.File, error) {
	return nil, nil
}
func (s *deleteRecordStorage) Delete(_ context.Context, key string) error {
	s.deleteCalls = append(s.deleteCalls, key)
	return s.deleteErr
}
func (s *deleteRecordStorage) Exists(context.Context, string) (bool, error) {
	return true, nil
}
func (s *deleteRecordStorage) GetSize(context.Context, string) (int64, error) { return 0, nil }
func (s *deleteRecordStorage) GetURL(context.Context, string) (string, error) { return "", nil }
func (s *deleteRecordStorage) List(context.Context, string) ([]storage.FileInfo, error) {
	return nil, nil
}
func (s *deleteRecordStorage) Close() error   { return nil }
func (s *deleteRecordStorage) GetKey() string { return s.key }

func newDeleteRecordTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(gormlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&RecordStream{}); err != nil {
		t.Fatalf("migrate RecordStream: %v", err)
	}
	return db
}

func newDeleteRecordTestServer(
	t *testing.T,
	db *gorm.DB,
	backends map[string]*deleteRecordStorage,
	active *deleteRecordStorage,
) *Server {
	t.Helper()
	configs := make(map[string]any, len(backends))
	for storageType, backend := range backends {
		storageType := storageType
		backend := backend
		originalFactory, existed := storage.Factory[storageType]
		storage.Factory[storageType] = func(any) (storage.Storage, error) {
			return backend, nil
		}
		t.Cleanup(func() {
			if existed {
				storage.Factory[storageType] = originalFactory
			} else {
				delete(storage.Factory, storageType)
			}
		})
		configs[storageType] = struct{}{}
	}
	registry := storage.NewRegistry(configs)
	t.Cleanup(func() { _ = registry.Close() })
	server := &Server{
		Plugin:         Plugin{DB: db},
		storageRuntime: &storageRuntime{registry: registry},
	}
	server.activateStorage(active, StorageStatus{ActiveType: active.GetKey()})
	return server
}

func createDeleteRecordFixtures(t *testing.T, db *gorm.DB, storageTypes ...string) []RecordStream {
	t.Helper()
	records := make([]RecordStream, 0, len(storageTypes))
	for index, storageType := range storageTypes {
		records = append(records, RecordStream{
			StreamPath:  "live/test",
			Type:        "mp4",
			StorageType: storageType,
			FilePath:    "records/segment-" + string(rune('a'+index)) + ".mp4",
		})
	}
	if err := db.Create(&records).Error; err != nil {
		t.Fatalf("create records: %v", err)
	}
	return records
}

func recordIDs(records []RecordStream) []uint32 {
	ids := make([]uint32, len(records))
	for index := range records {
		ids[index] = uint32(records[index].ID)
	}
	return ids
}

func TestDeleteRecordUsesEachPersistedStorageTypeWithoutActiveBackend(t *testing.T) {
	const (
		storageTypeA = "delete-archive-a"
		storageTypeB = "delete-archive-b"
	)
	db := newDeleteRecordTestDB(t)
	backendA := &deleteRecordStorage{key: storageTypeA}
	backendB := &deleteRecordStorage{key: storageTypeB}
	active := &deleteRecordStorage{key: "unrelated-active"}
	server := newDeleteRecordTestServer(t, db, map[string]*deleteRecordStorage{
		storageTypeA: backendA,
		storageTypeB: backendB,
	}, active)
	records := createDeleteRecordFixtures(t, db, storageTypeA, storageTypeB)

	response, err := server.DeleteRecord(context.Background(), &pb.ReqRecordDelete{
		StreamPath: "live/test",
		Type:       "mp4",
		Ids:        recordIDs(records),
	})

	if err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	if response == nil || len(response.Data) != 2 {
		t.Fatalf("response=%+v, want two deleted records", response)
	}
	if !reflect.DeepEqual(backendA.deleteCalls, []string{"records/segment-a.mp4"}) {
		t.Fatalf("backend A delete calls=%v", backendA.deleteCalls)
	}
	if !reflect.DeepEqual(backendB.deleteCalls, []string{"records/segment-b.mp4"}) {
		t.Fatalf("backend B delete calls=%v", backendB.deleteCalls)
	}
	if len(active.deleteCalls) != 0 {
		t.Fatalf("active backend must not be used, calls=%v", active.deleteCalls)
	}
	var remaining int64
	if err := db.Model(&RecordStream{}).Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("remaining records=%d, want 0", remaining)
	}
}

func TestDeleteRecordRollsBackDatabaseWhenPhysicalDeleteFails(t *testing.T) {
	const (
		storageType = "delete-failing-archive"
		secret      = "endpoint=https://operator:credential@storage.invalid?X-Amz-Signature=must-not-appear"
	)
	db := newDeleteRecordTestDB(t)
	deleteErr := errors.New(secret)
	backend := &deleteRecordStorage{key: storageType, deleteErr: deleteErr}
	active := &deleteRecordStorage{key: "unrelated-active"}
	server := newDeleteRecordTestServer(t, db, map[string]*deleteRecordStorage{
		storageType: backend,
	}, active)
	var logs bytes.Buffer
	server.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	records := createDeleteRecordFixtures(t, db, storageType)

	response, err := server.DeleteRecord(context.Background(), &pb.ReqRecordDelete{
		StreamPath: "live/test",
		Type:       "mp4",
		Ids:        recordIDs(records),
	})

	if !errors.Is(err, deleteErr) {
		t.Fatalf("error does not preserve physical delete cause")
	}
	observed := err.Error() + "\n" + logs.String()
	for _, forbidden := range []string{secret, "credential", "X-Amz-Signature"} {
		if strings.Contains(observed, forbidden) {
			t.Fatalf("storage detail %q escaped through API error or logs: %s", forbidden, observed)
		}
	}
	if response != nil {
		t.Fatalf("response=%+v, want nil on failure", response)
	}
	if !reflect.DeepEqual(backend.deleteCalls, []string{"records/segment-a.mp4"}) {
		t.Fatalf("backend delete calls=%v", backend.deleteCalls)
	}
	if len(active.deleteCalls) != 0 {
		t.Fatalf("active backend must not be used, calls=%v", active.deleteCalls)
	}
	var remaining int64
	if err := db.Model(&RecordStream{}).Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("remaining records=%d, want rollback to preserve 1", remaining)
	}
}

func TestDeleteRecordResolverFailureDoesNotExposeStorageDetails(t *testing.T) {
	const (
		storageType = "delete-unavailable-archive"
		secret      = "endpoint=https://operator:credential@storage.invalid?X-Amz-Signature=must-not-appear"
	)
	db := newDeleteRecordTestDB(t)
	originalFactory, existed := storage.Factory[storageType]
	storage.Factory[storageType] = func(any) (storage.Storage, error) {
		return nil, errors.New(secret)
	}
	t.Cleanup(func() {
		if existed {
			storage.Factory[storageType] = originalFactory
		} else {
			delete(storage.Factory, storageType)
		}
	})
	registry := storage.NewRegistry(map[string]any{storageType: struct{}{}})
	t.Cleanup(func() { _ = registry.Close() })
	var logs bytes.Buffer
	server := &Server{
		Plugin:         Plugin{DB: db},
		storageRuntime: &storageRuntime{registry: registry},
	}
	server.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	server.activateStorage(&deleteRecordStorage{key: "unrelated-active"}, StorageStatus{ActiveType: "unrelated-active"})
	records := createDeleteRecordFixtures(t, db, storageType)

	response, err := server.DeleteRecord(context.Background(), &pb.ReqRecordDelete{
		StreamPath: "live/test",
		Type:       "mp4",
		Ids:        recordIDs(records),
	})

	if err == nil {
		t.Fatal("DeleteRecord must fail when persisted storage cannot be resolved")
	}
	if response != nil {
		t.Fatalf("response=%+v, want nil on failure", response)
	}
	observed := err.Error() + "\n" + logs.String()
	for _, forbidden := range []string{secret, "credential", "X-Amz-Signature"} {
		if strings.Contains(observed, forbidden) {
			t.Fatalf("storage detail %q escaped through API error or logs: %s", forbidden, observed)
		}
	}
	var remaining int64
	if db.Model(&RecordStream{}).Count(&remaining).Error != nil || remaining != 1 {
		t.Fatalf("remaining records=%d, want rollback to preserve 1", remaining)
	}
}
