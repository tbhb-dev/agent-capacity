// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package claude //nolint:testpackage // Exercise bounded reads and malformed events without private inputs.

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.tbhb.dev/agent-capacity/internal/capacity"
)

const (
	transcriptEventLine = `{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":` +
		`{"seven_day":{"utilization":0.25,"resetsAt":200000,"status":"allowed"}}}}`
	transcriptAccount = "transcript-work"
	transcriptNull    = "null"
	transcriptInput   = "input"
	readBudget        = 8 * 1024 * 1024
	lineBudget        = 64 * 1024
)

func TestTranscriptParsing(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		line string
		want int
	}{
		{transcriptEventLine, 1},
		{strings.ReplaceAll(transcriptEventLine, "seven_day", "five_hour"), 1},
		{strings.ReplaceAll(transcriptEventLine, "0.25", "0"), 1},
		{strings.ReplaceAll(transcriptEventLine, "0.25", "1"), 1},
		{strings.ReplaceAll(transcriptEventLine, "0.25", "1.01"), 0},
		{strings.ReplaceAll(transcriptEventLine, "0.25", "-0.01"), 0},
		{strings.ReplaceAll(transcriptEventLine, "0.25", "null"), 0},
		{strings.ReplaceAll(transcriptEventLine, `"utilization":0.25,`, ""), 0},
		{strings.ReplaceAll(transcriptEventLine, `"resetsAt":200000,`, ""), 0},
		{strings.ReplaceAll(transcriptEventLine, "200000", "null"), 0},
		{strings.ReplaceAll(transcriptEventLine, "200000", `"invalid"`), 0},
		{strings.ReplaceAll(transcriptEventLine, `,"status":"allowed"`, ""), 0},
		{strings.ReplaceAll(transcriptEventLine, `"allowed"`, "null"), 0},
		{strings.ReplaceAll(transcriptEventLine, "rate_limit_event", "assistant"), 0},
		{`{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"seven_day":null}}}`, 0},
		{`{"type":"rate_limit_event","rate_limit_info":null}`, 0},
		{`{"type":"rate_limit_event"}`, 0},
		{`{`, 0},
		{`[]`, 0},
		{transcriptNull, 0},
	} {
		source := TranscriptSource{files: nil, paths: nil, account: transcriptAccount}
		latest := map[string]capacity.Reading{}
		source.considerTranscript([]byte(testCase.line), time.Unix(100000, 0).UTC(), time.Unix(100000, 0).UTC(), latest)
		require.Len(t, latest, testCase.want)
	}
}

func TestTranscriptSelection(t *testing.T) {
	t.Parallel()

	source := TranscriptSource{files: nil, paths: nil, account: transcriptAccount}
	latest := map[string]capacity.Reading{}
	observed := time.Unix(100000, 0).UTC()
	source.considerTranscript([]byte(transcriptEventLine), observed, observed, latest)
	assert.Equal(t, capacity.Reading{
		Vendor: "anthropic", Account: transcriptAccount, Source: capacity.ClaudeTranscript, Window: "7d",
		UsedRatio: 0.25, ResetAt: time.Unix(200000, 0).UTC(), ObservedAt: observed,
	}, latest["7d"])

	for _, timestamp := range []string{`"1970-01-02T03:46:41Z"`, `100001`} {
		line := strings.Replace(transcriptEventLine, `"type":`, `"timestamp":`+timestamp+`,"type":`, 1)
		line = strings.ReplaceAll(line, "0.25", "0.5")
		line = strings.ReplaceAll(line, `"resetsAt":200000`, `"reset_time":"1970-01-03T07:33:20Z"`)
		source.considerTranscript([]byte(line), observed.Add(time.Hour), observed.Add(time.Hour), latest)
		assert.Equal(t, observed.Add(time.Second), latest["7d"].ObservedAt)
		assert.InDelta(t, 0.5, latest["7d"].UsedRatio, 0)
		assert.Equal(t, time.Unix(200000, 0).UTC(), latest["7d"].ResetAt)
	}

	source.considerTranscript([]byte(transcriptEventLine), observed, observed, latest)
	assert.InDelta(t, 0.5, latest["7d"].UsedRatio, 0)

	sameTime := strings.ReplaceAll(transcriptEventLine, "0.25", "0.75")
	source.considerTranscript([]byte(sameTime), observed.Add(time.Second), observed.Add(time.Second), latest)
	assert.InDelta(t, 0.75, latest["7d"].UsedRatio, 0)

	line := strings.ReplaceAll(transcriptEventLine, "seven_day", "five_hour")
	source.considerTranscript([]byte(line), observed.Add(time.Second), observed.Add(time.Second), latest)
	require.Len(t, latest, 2)
	assert.Equal(t, "5h", latest["5h"].Window)
	assert.InDelta(t, 0.25, latest["5h"].UsedRatio, 0)
}

func TestTranscriptFiles(t *testing.T) {
	t.Parallel()

	observed := time.Unix(100000, 0).UTC()
	files := fstest.MapFS{
		"input/one.jsonl": {
			Data:    []byte("invalid\n" + transcriptEventLine + "\n"),
			ModTime: observed,
			Mode:    0,
			Sys:     nil,
		},
		"input/two.ndjson": {
			Data:    []byte(strings.ReplaceAll(transcriptEventLine, "seven_day", "five_hour")),
			ModTime: observed, Mode: 0, Sys: nil,
		},
		"input/ignore.json": {
			Data:    []byte(strings.ReplaceAll(transcriptEventLine, "0.25", "0.75")),
			ModTime: observed.Add(time.Hour), Mode: 0, Sys: nil,
		},
		"input/sub/nested.jsonl": {
			Data:    []byte(transcriptEventLine),
			ModTime: observed.Add(time.Hour),
			Mode:    0,
			Sys:     nil,
		},
		"input/link.jsonl": {
			Data:    []byte(transcriptEventLine),
			Mode:    fs.ModeSymlink,
			ModTime: observed.Add(time.Hour),
			Sys:     nil,
		},
		"empty": {Data: nil, ModTime: observed, Mode: 0, Sys: nil},
	}
	source := TranscriptSource{
		files:   files,
		paths:   []string{"missing", "empty", transcriptInput},
		account: transcriptAccount,
	}
	assert.Equal(t, []string{"input/one.jsonl", "input/two.ndjson"}, transcriptPaths(files, "input"))

	got, err := source.Read(t.Context(), observed.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "5h", got[0].Window)
	assert.Equal(t, "7d", got[1].Window)
	assert.Equal(t, observed, got[0].ObservedAt)
	assert.Equal(t, observed, got[1].ObservedAt)
	assert.InDelta(t, 0.25, got[1].UsedRatio, 0)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got, err = source.Read(ctx, observed)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, got)
}

func TestTranscriptBounds(t *testing.T) {
	t.Parallel()

	for _, prefix := range []string{
		strings.Repeat("x", lineBudget+1) + "\n",
		strings.Repeat("x", readBudget) + "\n",
		strings.Repeat("x", readBudget-len(transcriptEventLine)-1) + "\n",
	} {
		source := TranscriptSource{
			files: fstest.MapFS{
				transcriptInput: {Data: []byte(prefix + transcriptEventLine), Mode: 0, ModTime: time.Time{}, Sys: nil},
			},
			paths:   []string{transcriptInput},
			account: transcriptAccount,
		}
		got, err := source.Read(t.Context(), time.Time{})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.InDelta(t, 0.25, got[0].UsedRatio, 0)
	}

	// Ignore a valid JSON fragment where the tail starts inside a larger line.
	data := "x" + transcriptEventLine + "\n" + strings.Repeat(" ", readBudget-len(transcriptEventLine)-1)
	source := TranscriptSource{
		files:   fstest.MapFS{transcriptInput: {Data: []byte(data), Mode: 0, ModTime: time.Time{}, Sys: nil}},
		paths:   []string{transcriptInput},
		account: transcriptAccount,
	}
	got, err := source.Read(t.Context(), time.Time{})
	require.NoError(t, err)
	assert.Empty(t, got)

	// Accept the line limit itself, then reject a line one byte longer.
	for _, extra := range []int{0, 1} {
		padded := transcriptEventLine + strings.Repeat(" ", lineBudget-len(transcriptEventLine)+extra)
		source.files = fstest.MapFS{transcriptInput: {Data: []byte(padded), Mode: 0, ModTime: time.Time{}, Sys: nil}}
		got, err = source.Read(t.Context(), time.Time{})
		require.NoError(t, err)
		assert.Len(t, got, 1-extra)
	}
}

func TestTranscriptRegistration(t *testing.T) {
	t.Parallel()

	registry, err := RegisterTranscripts("", "")
	require.NoError(t, err)
	assert.Nil(t, registry)
	registry, err = RegisterTranscripts("unused", "private@example.invalid")
	require.ErrorIs(t, err, capacity.ErrInvalidReading)
	assert.Nil(t, registry)

	dir := t.TempDir()
	name := filepath.Join(dir, "one.jsonl")
	require.NoError(t, os.WriteFile(name, []byte(transcriptEventLine), 0o400))

	observed := time.Unix(100000, 0).UTC()
	require.NoError(t, os.Chtimes(name, observed, observed))
	registry, err = RegisterTranscripts(name+","+dir, transcriptAccount)
	require.NoError(t, err)
	require.Len(t, registry, 1)
	assert.Equal(t, capacity.ClaudeTranscript, registry[0].Name)
	got, failures := registry.Read(t.Context(), observed)
	assert.Empty(t, failures)
	require.Len(t, got, 1)
	assert.Equal(t, transcriptAccount, got[0].Account)

	data, err := os.ReadFile(name)
	require.NoError(t, err)
	assert.JSONEq(t, transcriptEventLine, string(data))
}

type transcriptFaultFS struct {
	fs.FS

	openErr  bool
	statErr  bool
	readErr  bool
	noReader bool
}

func (files transcriptFaultFS) Open(name string) (fs.File, error) {
	if files.openErr {
		return nil, fs.ErrPermission
	}

	file, err := files.FS.Open(name)
	if err != nil {
		return nil, errors.Join(err)
	}

	wrapped := transcriptFaultFile{File: file, statErr: files.statErr, readErr: files.readErr}
	if files.noReader {
		return struct{ fs.File }{File: wrapped}, nil
	}

	return wrapped, nil
}

type transcriptFaultFile struct {
	fs.File

	statErr bool
	readErr bool
}

func (file transcriptFaultFile) Stat() (fs.FileInfo, error) {
	if file.statErr {
		return nil, fs.ErrPermission
	}

	info, err := file.File.Stat()

	return info, errors.Join(err)
}

func (file transcriptFaultFile) ReadAt(data []byte, offset int64) (int, error) {
	if file.readErr {
		return copy(data, transcriptEventLine), io.ErrUnexpectedEOF
	}

	reader, ok := file.File.(io.ReaderAt)
	if !ok {
		return 0, errors.ErrUnsupported
	}

	count, err := reader.ReadAt(data, offset)

	return count, errors.Join(err)
}

func TestTranscriptReadFailures(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{transcriptInput: {Data: []byte(transcriptEventLine), Mode: 0, ModTime: time.Time{}, Sys: nil}}
	for _, fixture := range []transcriptFaultFS{
		{FS: files, openErr: true, statErr: false, readErr: false, noReader: false},
		{FS: files, openErr: false, statErr: true, readErr: false, noReader: false},
		{FS: files, openErr: false, statErr: false, readErr: true, noReader: false},
		{FS: files, openErr: false, statErr: false, readErr: false, noReader: true},
		{FS: files, openErr: false, statErr: false, readErr: false, noReader: false},
	} {
		source := TranscriptSource{files: fixture, paths: []string{transcriptInput}, account: transcriptAccount}
		latest := map[string]capacity.Reading{}
		source.readTranscript(transcriptInput, time.Time{}, latest)

		if fixture.openErr || fixture.statErr || fixture.readErr || fixture.noReader {
			assert.Empty(t, latest)
		} else {
			require.Len(t, latest, 1)
		}
		// The unwrapped directory path also exercises a failed ReadDir.
		source.paths = []string{"."}
		got, err := source.Read(t.Context(), time.Time{})
		require.NoError(t, err)
		assert.Empty(t, got)
	}

	source := TranscriptSource{files: files, paths: nil, account: transcriptAccount}
	latest := map[string]capacity.Reading{}
	source.readTranscript(".", time.Time{}, latest)
	assert.Empty(t, latest)

	files["link"] = &fstest.MapFile{
		Data:    []byte(transcriptEventLine),
		Mode:    fs.ModeSymlink,
		ModTime: time.Time{},
		Sys:     nil,
	}

	source.readTranscript("link", time.Time{}, latest)
	assert.Empty(t, latest)
}

func TestTranscriptIndependentWindows(t *testing.T) {
	t.Parallel()

	source := TranscriptSource{files: nil, account: transcriptAccount, paths: nil}

	for _, invalid := range []string{
		transcriptNull,
		`{"utilization":null,"resetsAt":200000,"status":"allowed"}`,
		`{"utilization":0.5,"resetsAt":null,"status":"allowed"}`,
		`{"utilization":1.1,"resetsAt":200000,"status":"allowed"}`,
	} {
		line := strings.Replace(
			transcriptEventLine,
			`"unifiedWindows":{`,
			`"unifiedWindows":{"five_hour":`+invalid+`,`,
			1,
		)
		latest := map[string]capacity.Reading{}
		source.considerTranscript([]byte(line), time.Time{}.Add(-time.Nanosecond), time.Time{}, latest)
		require.Len(t, latest, 1)
		assert.InDelta(t, 0.25, latest["7d"].UsedRatio, 0)
	}
}

type partialDirectoryFS struct{ fs.FS }

func (files partialDirectoryFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(files.FS, name)

	return entries, errors.Join(err, fs.ErrPermission)
}

func TestTranscriptDirectoryFailure(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"dir/hidden.jsonl": {
			Data:    []byte(strings.ReplaceAll(transcriptEventLine, "0.25", "0.75")),
			Mode:    0,
			ModTime: time.Unix(100001, 0),
			Sys:     nil,
		},
		"visible": {Data: []byte(transcriptEventLine), Mode: 0, ModTime: time.Unix(100000, 0), Sys: nil},
	}
	source := TranscriptSource{
		files:   partialDirectoryFS{FS: files},
		account: transcriptAccount,
		paths:   []string{"dir", "visible"},
	}
	got, err := source.Read(t.Context(), time.Unix(100000, 0))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.InDelta(t, 0.25, got[0].UsedRatio, 0)
}

type transcriptProbeFS struct {
	fs.FS

	readAt func([]byte, int64) (int, error)
	mode   fs.FileMode
}

func (files transcriptProbeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(files.FS, name)

	return entries, errors.Join(err)
}

func (files transcriptProbeFS) Open(name string) (fs.File, error) {
	file, err := files.FS.Open(name)

	return transcriptProbeFile{File: file, readAt: files.readAt, mode: files.mode}, errors.Join(err)
}

type transcriptProbeFile struct {
	fs.File

	readAt func([]byte, int64) (int, error)
	mode   fs.FileMode
}

func (file transcriptProbeFile) ReadAt(data []byte, offset int64) (int, error) {
	return file.readAt(data, offset)
}

func TestTranscriptReadBudget(t *testing.T) {
	t.Parallel()

	calls := 0
	files := transcriptProbeFS{
		mode: 0,
		FS: fstest.MapFS{
			transcriptInput: {Data: make([]byte, readBudget+2), Mode: 0, ModTime: time.Time{}, Sys: nil},
		},
		readAt: func(data []byte, offset int64) (int, error) {
			calls++

			assert.Len(t, data, 8388608)
			assert.Equal(t, int64(2), offset)

			return len(data), nil
		},
	}
	source := TranscriptSource{files: files, account: transcriptAccount, paths: []string{transcriptInput}}
	got, err := source.Read(t.Context(), time.Time{})
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, 1, calls)
}

type transcriptModeInfo struct {
	fs.FileInfo

	mode fs.FileMode
}

func (info transcriptModeInfo) Mode() fs.FileMode {
	return info.mode
}

func (file transcriptProbeFile) Stat() (fs.FileInfo, error) {
	info, err := file.File.Stat()

	return transcriptModeInfo{FileInfo: info, mode: file.mode}, errors.Join(err)
}

func TestTranscriptSpecialFile(t *testing.T) {
	t.Parallel()

	calls := 0
	files := transcriptProbeFS{
		FS: fstest.MapFS{
			transcriptInput: {Data: []byte(transcriptEventLine), Mode: 0, ModTime: time.Time{}, Sys: nil},
		},
		mode: fs.ModeNamedPipe,
		readAt: func(data []byte, _ int64) (int, error) {
			calls++

			return copy(data, transcriptEventLine), nil
		},
	}
	source := TranscriptSource{files: files, paths: nil, account: transcriptAccount}
	latest := map[string]capacity.Reading{}
	source.readTranscript(transcriptInput, time.Time{}, latest)
	assert.Empty(t, latest)
	assert.Equal(t, 0, calls)
}

func TestTranscriptOpenFailure(t *testing.T) {
	t.Parallel()

	file, err := (transcriptFiles{}).Open(filepath.Join(t.TempDir(), "missing"))
	require.ErrorIs(t, err, fs.ErrNotExist)
	assert.Nil(t, file)
}

func TestTranscriptDirectoryAgeBudget(t *testing.T) {
	t.Parallel()

	now := time.Unix(100000, 0).UTC()
	calls := 0
	fixtures := fstest.MapFS{}

	for name, age := range map[string]time.Duration{
		"old.jsonl":      6*time.Hour + time.Nanosecond,
		"boundary.jsonl": 6 * time.Hour,
		"recent.ndjson":  6*time.Hour - time.Nanosecond,
	} {
		fixtures["dir/"+name] = &fstest.MapFile{
			Data: []byte(transcriptEventLine), Mode: 0, Sys: nil, ModTime: now.Add(-age),
		}
	}

	files := transcriptProbeFS{
		FS:   fixtures,
		mode: 0,
		readAt: func(data []byte, offset int64) (int, error) {
			calls++

			assert.Zero(t, offset)

			return copy(data, transcriptEventLine), nil
		},
	}
	source := TranscriptSource{files: files, paths: []string{"dir"}, account: transcriptAccount}
	got, err := source.Read(t.Context(), now)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, now.Add(-6*time.Hour+time.Nanosecond), got[0].ObservedAt)
	assert.Equal(t, 2, calls)
}

func TestTranscriptFutureBoundary(t *testing.T) {
	t.Parallel()

	const (
		realPath   = "real"
		futurePath = "future"
	)

	now := time.Unix(100000, 0).UTC()

	for _, ahead := range []time.Duration{
		5*time.Minute - time.Nanosecond,
		5 * time.Minute,
		5*time.Minute + time.Nanosecond,
		24 * time.Hour,
	} {
		for _, explicit := range []bool{false, true} {
			future := strings.ReplaceAll(transcriptEventLine, "0.25", "0.75")
			modified := now.Add(ahead)

			if explicit {
				future = strings.Replace(
					future,
					`"type":`,
					`"timestamp":"`+modified.Format(time.RFC3339Nano)+`","type":`,
					1,
				)
				modified = now
			}

			files := fstest.MapFS{
				realPath:   {Data: []byte(transcriptEventLine), ModTime: now, Mode: 0, Sys: nil},
				futurePath: {Data: []byte(future), ModTime: modified, Mode: 0, Sys: nil},
			}
			for _, paths := range [][]string{{realPath, futurePath}, {futurePath, realPath}} {
				source := TranscriptSource{files: files, paths: paths, account: transcriptAccount}
				got, err := source.Read(t.Context(), now)
				require.NoError(t, err)

				require.Len(t, got, 1)

				if ahead <= 5*time.Minute {
					assert.InDelta(t, 0.75, got[0].UsedRatio, 0)
					assert.Equal(t, now.Add(ahead), got[0].ObservedAt)
				} else {
					assert.InDelta(t, 0.25, got[0].UsedRatio, 0)
					assert.Equal(t, now, got[0].ObservedAt)
				}
			}
		}
	}
}

func TestTranscriptNamedSymlinks(t *testing.T) {
	t.Parallel()

	now := time.Unix(100000, 0).UTC()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.Mkdir(target, 0o700))
	name := filepath.Join(target, "events.jsonl")
	require.NoError(t, os.WriteFile(name, []byte(transcriptEventLine), 0o600))
	require.NoError(t, os.Chtimes(name, now, now))

	fileLink := filepath.Join(dir, "file-link")
	dirLink := filepath.Join(dir, "dir-link")

	require.NoError(t, os.Symlink(name, fileLink))
	require.NoError(t, os.Symlink(target, dirLink))

	for _, path := range []string{fileLink, dirLink} {
		registry, err := RegisterTranscripts(path, transcriptAccount)
		require.NoError(t, err)
		got, failures := registry.Read(t.Context(), now)
		assert.Empty(t, failures)

		require.Len(t, got, 1)
		assert.Equal(t, now, got[0].ObservedAt.UTC())
	}
}
