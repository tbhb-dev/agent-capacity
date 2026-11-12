// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package codex //nolint:testpackage // Fault injection checks file errors and tail bounds.

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.tbhb.dev/agent-capacity/internal/capacity"
)

const (
	testChunkSize  = 64 * 1024
	testByteLimit  = 8 * 1024 * 1024
	rollout        = "sessions/2026/01/02/rollout-example.jsonl"
	observed       = "2026-01-02T03:04:05Z"
	weekly         = `{"primary":{"used_percent":25,"window_minutes":10080,"resets_at":2000000000},"secondary":null}`
	short          = `{"primary":{"used_percent":75,"window_minutes":300}}`
	primaryAccount = "primary"
	privateMarker  = "synthetic-private-content"
)

func eventLine(timestamp, snapshot string) string {
	return fmt.Sprintf(
		`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":%s,"text":%q},"unrelated":%q}`,
		timestamp,
		snapshot,
		privateMarker,
		privateMarker,
	)
}

func writeRollout(t *testing.T, home, name, content string) {
	t.Helper()

	name = filepath.Join(home, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
	require.NoError(t, os.WriteFile(name, []byte(content), 0o600))
}

func readHome(t *testing.T, home string) ([]capacity.Reading, error) {
	t.Helper()

	source := Source{files: checkedFS{FS: os.DirFS(home), t: t}, account: "work_2-a"}

	return source.Read(&budgetContext{remaining: 1000}, time.Time{})
}

func wantReading(window string, ratio float64, reset time.Time) capacity.Reading {
	timestamp := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

	return capacity.Reading{
		Vendor: "openai", Window: window, Account: "work_2-a", Source: capacity.CodexRollout,
		UsedRatio: ratio, ObservedAt: timestamp, ResetAt: reset,
	}
}

func TestRegister(t *testing.T) {
	t.Parallel()

	for _, home := range []string{"", t.TempDir()} {
		registry, err := Register(home, primaryAccount)
		require.NoError(t, err)

		if home == "" {
			assert.Nil(t, registry)
		} else {
			require.Len(t, registry, 1)
			assert.Equal(t, capacity.CodexRollout, registry[0].Name)
			readings, failures := registry.Read(t.Context(), time.Time{})
			assert.Empty(t, readings)
			assert.Empty(t, failures)
		}
	}

	for _, account := range []string{"", "UPPER", "synthetic@invalid", strings.Repeat("a", 33)} {
		registry, err := Register("", account)
		require.ErrorIs(t, err, capacity.ErrInvalidReading)
		assert.Nil(t, registry)
	}
}

func TestWindows(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name   string
		limits string
		want   []capacity.Reading
	}{
		{"weekly only", weekly, []capacity.Reading{wantReading("7d", 0.25, time.Unix(2000000000, 0))}},
		{"short only", short, []capacity.Reading{wantReading("5h", 0.75, time.Time{})}},
		{
			name: "both reversed",
			limits: `{"primary":{"used_percent":100,"window_minutes":10080},` +
				`"secondary":{"used_percent":0,"window_minutes":300,"resets_at":0}}`,
			want: []capacity.Reading{wantReading("7d", 1, time.Time{}), wantReading("5h", 0, time.Unix(0, 0))},
		},
		{
			name: "both usual",
			limits: `{"primary":{"used_percent":20,"window_minutes":300},` +
				`"secondary":{"used_percent":50,"window_minutes":10080}}`,
			want: []capacity.Reading{wantReading("5h", 0.2, time.Time{}), wantReading("7d", 0.5, time.Time{})},
		},
		{"unknown", `{"primary":{"used_percent":99,"window_minutes":60},"secondary":{"window_minutes":0}}`, nil},
		{"absent", `{"primary":null,"secondary":null}`, nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			writeRollout(t, home, rollout, eventLine(observed, testCase.limits))
			got, err := readHome(t, home)
			require.NoError(t, err)
			// Full equality also checks that readings omit unrelated input.
			assert.Equal(t, testCase.want, got)
		})
	}
}

func TestSkipLines(t *testing.T) {
	t.Parallel()

	for _, skipped := range []string{
		`malformed ` + privateMarker,
		`{"timestamp":"invalid","type":"event_msg"}`,
		eventLine("", weekly),
		`{"type":"event_msg","payload":{"type":"token_count","rate_limits":` + short + `}}`,
		eventLine("2026-01-03T00:00:00Z", `{"primary":{"used_percent":99,"window_minutes":300},"secondary":42}`),
		eventLine("2026-01-03T00:00:00Z", "null"),
		strings.ReplaceAll(eventLine(observed, short), "event_msg", "other"),
		strings.ReplaceAll(eventLine(observed, short), "token_count", "other"),
		eventLine("2026-01-03T00:00:00Z", `{"limit_name":"Other","primary":{"used_percent":80,"window_minutes":300}}`),
		eventLine("2026-01-03T00:00:00Z", `{"limit_name":"codex","primary":{"used_percent":80,"window_minutes":300}}`),
	} {
		home := t.TempDir()
		writeRollout(t, home, rollout, eventLine(observed, weekly)+"\n"+skipped+"\n")
		got, err := readHome(t, home)
		require.NoError(t, err)
		assert.Equal(t, []capacity.Reading{wantReading("7d", 0.25, time.Unix(2000000000, 0))}, got)
	}
}

func TestLatestAcrossFiles(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	// A family ID change replaces the whole snapshot, with no window carryover.
	writeRollout(t, home, rollout, eventLine(observed, strings.Replace(weekly, "{", `{"limit_id":"old",`, 1)))

	newestName := "sessions/2026/01/03/rollout-new.jsonl"
	writeRollout(
		t,
		home,
		newestName,
		eventLine(
			"2026-01-03T00:00:00Z",
			`{"limit_id":"new","limit_name":"Codex","secondary":{"used_percent":75,"window_minutes":300}}`,
		)+"\n"+eventLine(
			observed,
			weekly,
		),
	)
	require.NoError(
		t,
		os.Chtimes(filepath.Join(home, filepath.FromSlash(newestName)), time.Unix(1, 0), time.Unix(1, 0)),
	)
	got, err := readHome(t, home)
	require.NoError(t, err)

	want := wantReading("5h", 0.75, time.Time{})
	want.ObservedAt = time.Date(2026, time.January, 3, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, []capacity.Reading{want}, got)
}

func TestEqualTimestamps(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeRollout(t, home, rollout, eventLine(observed, weekly)+"\n"+eventLine(observed, short))
	got, err := readHome(t, home)
	require.NoError(t, err)
	assert.Equal(t, []capacity.Reading{wantReading("5h", 0.75, time.Time{})}, got)
}

type trackingReader struct {
	reader  io.ReaderAt
	offsets []int64
	lengths []int
}

func (r *trackingReader) ReadAt(data []byte, offset int64) (int, error) {
	if len(data) == 0 {
		return 0, io.ErrUnexpectedEOF
	}

	r.offsets = append(r.offsets, offset)
	r.lengths = append(r.lengths, len(data))

	count, err := r.reader.ReadAt(data, offset)
	if err != nil {
		return count, safeError(err)
	}

	return count, nil
}

func TestChunkLimits(t *testing.T) {
	t.Parallel()
	// Check chunk lengths and offsets without a snapshot.
	reader := &trackingReader{
		reader: strings.NewReader(strings.Repeat(" ", testByteLimit+1)), offsets: nil, lengths: nil,
	}

	var latest snapshot
	require.NoError(t, scanTail(&budgetContext{remaining: 1000}, reader, testByteLimit+1, &latest))
	require.Len(t, reader.offsets, testByteLimit/testChunkSize)

	for index, offset := range reader.offsets {
		assert.Equal(t, int64(testByteLimit+1-(index+1)*testChunkSize), offset)
		assert.Equal(t, testChunkSize, reader.lengths[index])
	}

	assert.Nil(t, latest.limits)
}

func TestFileSelection(t *testing.T) {
	t.Parallel()
	home := t.TempDir()

	for index := range 6 {
		name := fmt.Sprintf("sessions/2026/01/02/rollout-%d.jsonl", index)

		content := eventLine(observed, short)
		if index == 0 {
			content = eventLine("2027-01-01T00:00:00Z", weekly)
		}

		writeRollout(t, home, name, content)

		modified := time.Unix(int64(index), 0)
		require.NoError(t, os.Chtimes(filepath.Join(home, filepath.FromSlash(name)), modified, modified))
	}

	for _, name := range []string{
		"sessions/2026/01/02/unrelated.jsonl",
		"sessions/other/rollout-example.jsonl",
		"sessions/2026/01/02/rollout-example.txt",
	} {
		writeRollout(t, home, name, eventLine("2028-01-01T00:00:00Z", weekly))
	}

	got, err := readHome(t, home)
	require.NoError(t, err)
	assert.Equal(t, []capacity.Reading{wantReading("5h", 0.75, time.Time{})}, got)
	// Equal modification times use the filename to make selection stable.
	files, err := newest(os.DirFS(home))
	require.NoError(t, err)
	require.Len(t, files, 5)
	assert.Equal(t, "sessions/2026/01/02/rollout-5.jsonl", files[0].name)

	for _, file := range files {
		require.NoError(
			t,
			os.Chtimes(filepath.Join(home, filepath.FromSlash(file.name)), time.Unix(10, 0), time.Unix(10, 0)),
		)
	}

	files, err = newest(os.DirFS(home))
	require.NoError(t, err)
	assert.Equal(t, "sessions/2026/01/02/rollout-1.jsonl", files[0].name)
}

func TestTailBounds(t *testing.T) {
	t.Parallel()

	for _, padding := range []int{testChunkSize - 20, testChunkSize, testByteLimit, testByteLimit + testChunkSize} {
		home := t.TempDir()
		content := eventLine(
			"2027-01-01T00:00:00Z",
			weekly,
		) + "\n" + strings.Repeat(
			"x",
			padding,
		) + "\n" + eventLine(
			observed,
			short,
		)
		writeRollout(t, home, rollout, content)
		got, err := readHome(t, home)
		require.NoError(t, err)

		if padding < testByteLimit {
			want := wantReading("7d", 0.25, time.Unix(2000000000, 0))
			want.ObservedAt = time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)
			assert.Equal(t, []capacity.Reading{want}, got)
		} else {
			assert.Equal(t, []capacity.Reading{wantReading("5h", 0.75, time.Time{})}, got)
		}
	}
	// A partial first line at the byte cap must never become a snapshot.
	line := eventLine(observed, short)

	var latest snapshot
	require.NoError(
		t,
		scanTail(
			&budgetContext{remaining: 1000},
			strings.NewReader("x"+line+strings.Repeat(" ", testByteLimit-len(line))),
			testByteLimit+1,
			&latest,
		),
	)
	assert.Nil(t, latest.limits)

	reader := strings.NewReader("x\n" + line + strings.Repeat(" ", testByteLimit-len(line)-1))
	require.NoError(t, scanTail(&budgetContext{remaining: 1000}, reader, testByteLimit+1, &latest))
	got, err := latest.readings("work_2-a")
	require.NoError(t, err)
	assert.Equal(t, []capacity.Reading{wantReading("5h", 0.75, time.Time{})}, got)
}

func TestZeroTimestamp(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeRollout(t, home, rollout, eventLine("0001-01-01T00:00:00Z", short))
	got, err := readHome(t, home)
	require.NoError(t, err)

	want := wantReading("5h", 0.75, time.Time{})
	want.ObservedAt = time.Time{}
	assert.Equal(t, []capacity.Reading{want}, got)
}

type budgetContext struct {
	remaining int
}

func (*budgetContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*budgetContext) Done() <-chan struct{}       { return nil }
func (*budgetContext) Value(any) any               { return nil }

func (c *budgetContext) Err() error {
	c.remaining--
	if c.remaining < 0 {
		return context.Canceled
	}

	return nil
}

func TestCancelDuringChunk(t *testing.T) {
	t.Parallel()

	ctx := &budgetContext{remaining: 1}

	var latest snapshot
	require.ErrorIs(t, scanTail(ctx, strings.NewReader("\n"), 1, &latest), context.Canceled)
	assert.Nil(t, latest.limits)
}

type checkedFS struct {
	fs.FS

	t *testing.T
}

func (f checkedFS) Open(name string) (fs.File, error) {
	file, err := f.FS.Open(name)
	if err != nil {
		return nil, fmt.Errorf("fixture open: %w", err)
	}

	osFile, ok := file.(*os.File)
	require.True(f.t, ok)

	return &checkedFile{File: osFile, t: f.t, bytes: 0}, nil
}

type checkedFile struct {
	*os.File

	t     *testing.T
	bytes int
}

func (f *checkedFile) ReadAt(data []byte, offset int64) (int, error) {
	f.t.Helper()
	require.NotEmpty(f.t, data)
	require.LessOrEqual(f.t, len(data), testChunkSize)
	f.bytes += len(data)
	require.LessOrEqual(f.t, f.bytes, testByteLimit)

	count, err := f.File.ReadAt(data, offset)
	if err != nil {
		return count, safeError(err)
	}

	return count, nil
}

func TestEmptyAndInvalid(t *testing.T) {
	t.Parallel()

	for _, content := range []string{"", "\n", eventLine(observed, "null"), "malformed"} {
		home := t.TempDir()
		writeRollout(t, home, rollout, content)
		got, err := readHome(t, home)
		require.NoError(t, err)
		assert.Nil(t, got)
	}

	home := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(home, "sessions"), 0o700))
	got, err := readHome(t, home)
	require.NoError(t, err)
	assert.Nil(t, got)
	require.NoError(t, os.MkdirAll(filepath.Join(home, filepath.FromSlash(rollout)), 0o700))
	got, err = readHome(t, home)
	require.NoError(t, err)
	assert.Nil(t, got)
	require.NoError(t, os.Remove(filepath.Join(home, filepath.FromSlash(rollout))))

	for _, snapshot := range []string{
		`{"primary":{"window_minutes":300}}`,
		`{"primary":{"window_minutes":300,"used_percent":-1}}`,
		`{"primary":{"window_minutes":300,"used_percent":101}}`,
		`{"primary":{"window_minutes":300,"used_percent":20},"secondary":{"window_minutes":10080}}`,
	} {
		writeRollout(t, home, rollout, eventLine(observed, snapshot))
		got, err = readHome(t, home)
		require.ErrorIs(t, err, capacity.ErrInvalidReading)
		assert.Nil(t, got)
		assert.NotContains(t, err.Error(), privateMarker)
	}
}

type faultFS struct {
	fs.FS

	err error
}

func (f faultFS) Open(string) (fs.File, error) { return nil, f.err }

type faultFile struct {
	fs.File

	err error
}

func (f faultFile) Stat() (fs.FileInfo, error) { return nil, f.err }
func (faultFile) Close() error                 { return nil }

type oneFileFS struct {
	fs.FS

	file fs.File
}

func (f oneFileFS) Open(string) (fs.File, error) { return f.file, nil }

type faultReader struct{}

func (faultReader) ReadAt([]byte, int64) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestSafeErrors(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		err  error
		want error
	}{
		{fs.ErrNotExist, nil},
		{fs.ErrPermission, fs.ErrPermission},
		{context.Canceled, context.Canceled},
		{context.DeadlineExceeded, context.DeadlineExceeded},
		{fmt.Errorf("%s: %w", privateMarker, io.ErrUnexpectedEOF), errRead},
	} {
		source := Source{files: faultFS{FS: nil, err: testCase.err}, account: primaryAccount}
		got, err := source.Read(t.Context(), time.Time{})
		assert.Nil(t, got)
		require.ErrorIs(t, err, testCase.want)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	source := Source{files: nil, account: primaryAccount}
	got, err := source.Read(ctx, time.Time{})
	assert.Nil(t, got)
	require.ErrorIs(t, err, context.Canceled)

	var latest snapshot
	require.ErrorIs(t, scanTail(ctx, nil, 1, &latest), context.Canceled)
	require.ErrorIs(t, scanTail(t.Context(), faultReader{}, 1, &latest), errRead)
	require.ErrorIs(
		t,
		sourceWithFile(faultFile{File: nil, err: fs.ErrPermission}).readTail(t.Context(), "unused", &latest),
		fs.ErrPermission,
	)
	source.files = faultFS{FS: nil, err: fs.ErrPermission}
	require.ErrorIs(t, source.readTail(t.Context(), "unused", &latest), fs.ErrPermission)
}

func sourceWithFile(file fs.File) Source {
	return Source{files: oneFileFS{FS: nil, file: file}, account: primaryAccount}
}

type selectiveFS struct {
	fs.FS
}

func (f selectiveFS) Open(name string) (fs.File, error) {
	if name == rollout {
		return nil, fs.ErrPermission
	}

	file, err := f.FS.Open(name)
	if err != nil {
		return nil, safeError(err)
	}

	return file, nil
}

type infoFailureFS struct{ fs.FS }

func (f infoFailureFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(f.FS, name)
	if err != nil {
		return nil, safeError(err)
	}

	for index, entry := range entries {
		entries[index] = infoFailureEntry{DirEntry: entry}
	}

	return entries, nil
}

type infoFailureEntry struct{ fs.DirEntry }

func (infoFailureEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrPermission }

type sequentialFile struct{ fs.File }

func TestFileErrors(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeRollout(t, home, rollout, eventLine(observed, weekly))

	for _, files := range []fs.FS{selectiveFS{FS: os.DirFS(home)}, infoFailureFS{FS: os.DirFS(home)}} {
		source := Source{files: files, account: primaryAccount}
		got, err := source.Read(t.Context(), time.Time{})
		require.ErrorIs(t, err, fs.ErrPermission)
		assert.Nil(t, got)
	}

	file, err := os.Open(filepath.Join(home, filepath.FromSlash(rollout)))
	require.NoError(t, err)

	var latest snapshot
	require.ErrorIs(t, sourceWithFile(sequentialFile{File: file}).readTail(t.Context(), "unused", &latest), errRead)
	other := t.TempDir()
	writeRollout(t, other, "sessions", "unreadable directory")
	got, err := readHome(t, other)
	require.ErrorIs(t, err, errRead)
	assert.Nil(t, got)
}
