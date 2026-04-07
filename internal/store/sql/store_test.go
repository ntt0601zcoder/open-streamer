package sql_test

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/store"
	sqlstore "github.com/ntthuan060102github/open-streamer/internal/store/sql"
	"github.com/ntthuan060102github/open-streamer/internal/store/storetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// requireDocker skips the test if the Docker daemon is not reachable.
func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("Docker not available:", err)
	}
}

func newSQLStore(t *testing.T) *sqlstore.Store {
	t.Helper()
	requireDocker(t)

	ctx := context.Background()
	ctr, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("testdb"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	s, err := sqlstore.New(dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	migrateCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	require.NoError(t, s.Migrate(migrateCtx))

	return s
}

// --- StreamRepository ---

func TestSQLStreamRepo_SaveAndFindByCode(t *testing.T) {
	ctx := context.Background()
	repo := newSQLStore(t).Streams()

	want := storetest.NewFullStream("teststreamA")
	require.NoError(t, repo.Save(ctx, want))

	got, err := repo.FindByCode(ctx, "teststreamA")
	require.NoError(t, err)

	assert.Equal(t, want.Code, got.Code)
	assert.Equal(t, want.Name, got.Name)
	assert.Equal(t, want.Description, got.Description)
	assert.Equal(t, want.Tags, got.Tags)
	assert.Equal(t, want.StreamKey, got.StreamKey)
	assert.Equal(t, want.Status, got.Status)
	assert.Equal(t, want.Disabled, got.Disabled)

	require.Len(t, got.Inputs, 2)
	assert.Equal(t, want.Inputs[0].URL, got.Inputs[0].URL)
	assert.Equal(t, want.Inputs[0].Priority, got.Inputs[0].Priority)
	assert.Equal(t, want.Inputs[0].Headers, got.Inputs[0].Headers)
	assert.Equal(t, want.Inputs[0].Params, got.Inputs[0].Params)
	assert.Equal(t, want.Inputs[0].Net.ConnectTimeoutSec, got.Inputs[0].Net.ConnectTimeoutSec)
	assert.Equal(t, want.Inputs[0].Net.ReadTimeoutSec, got.Inputs[0].Net.ReadTimeoutSec)
	assert.Equal(t, want.Inputs[0].Net.Reconnect, got.Inputs[0].Net.Reconnect)
	assert.Equal(t, want.Inputs[0].Net.ReconnectDelaySec, got.Inputs[0].Net.ReconnectDelaySec)
	assert.Equal(t, want.Inputs[0].Net.ReconnectMaxDelaySec, got.Inputs[0].Net.ReconnectMaxDelaySec)
	assert.Equal(t, want.Inputs[0].Net.MaxReconnects, got.Inputs[0].Net.MaxReconnects)

	require.NotNil(t, got.Transcoder)
	assert.Equal(t, want.Transcoder.Video.Copy, got.Transcoder.Video.Copy)
	require.Len(t, got.Transcoder.Video.Profiles, 2)
	p0 := got.Transcoder.Video.Profiles[0]
	assert.Equal(t, want.Transcoder.Video.Profiles[0].Width, p0.Width)
	assert.Equal(t, want.Transcoder.Video.Profiles[0].Height, p0.Height)
	assert.Equal(t, want.Transcoder.Video.Profiles[0].Bitrate, p0.Bitrate)
	assert.Equal(t, want.Transcoder.Video.Profiles[0].MaxBitrate, p0.MaxBitrate)
	assert.Equal(t, want.Transcoder.Video.Profiles[0].Framerate, p0.Framerate)
	assert.Equal(t, want.Transcoder.Video.Profiles[0].KeyframeInterval, p0.KeyframeInterval)
	assert.Equal(t, want.Transcoder.Video.Profiles[0].Codec, p0.Codec)
	assert.Equal(t, want.Transcoder.Video.Profiles[0].Preset, p0.Preset)
	assert.Equal(t, want.Transcoder.Video.Profiles[0].Profile, p0.Profile)
	assert.Equal(t, want.Transcoder.Video.Profiles[0].Level, p0.Level)

	assert.Equal(t, want.Transcoder.Audio.Copy, got.Transcoder.Audio.Copy)
	assert.Equal(t, want.Transcoder.Audio.Codec, got.Transcoder.Audio.Codec)
	assert.Equal(t, want.Transcoder.Audio.Bitrate, got.Transcoder.Audio.Bitrate)
	assert.Equal(t, want.Transcoder.Audio.SampleRate, got.Transcoder.Audio.SampleRate)
	assert.Equal(t, want.Transcoder.Audio.Channels, got.Transcoder.Audio.Channels)
	assert.Equal(t, want.Transcoder.Audio.Language, got.Transcoder.Audio.Language)
	assert.Equal(t, want.Transcoder.Audio.Normalize, got.Transcoder.Audio.Normalize)

	assert.Equal(t, want.Transcoder.Decoder.Name, got.Transcoder.Decoder.Name)
	assert.Equal(t, want.Transcoder.Global.HW, got.Transcoder.Global.HW)
	assert.Equal(t, want.Transcoder.Global.FPS, got.Transcoder.Global.FPS)
	assert.Equal(t, want.Transcoder.Global.GOP, got.Transcoder.Global.GOP)
	assert.Equal(t, want.Transcoder.Global.DeviceID, got.Transcoder.Global.DeviceID)
	assert.Equal(t, want.Transcoder.ExtraArgs, got.Transcoder.ExtraArgs)

	assert.Equal(t, want.Protocols, got.Protocols)

	require.Len(t, got.Push, 1)
	assert.Equal(t, want.Push[0].URL, got.Push[0].URL)
	assert.Equal(t, want.Push[0].Enabled, got.Push[0].Enabled)
	assert.Equal(t, want.Push[0].TimeoutSec, got.Push[0].TimeoutSec)
	assert.Equal(t, want.Push[0].RetryTimeoutSec, got.Push[0].RetryTimeoutSec)
	assert.Equal(t, want.Push[0].Limit, got.Push[0].Limit)
	assert.Equal(t, want.Push[0].Comment, got.Push[0].Comment)

	require.NotNil(t, got.DVR)
	assert.Equal(t, want.DVR.Enabled, got.DVR.Enabled)
	assert.Equal(t, want.DVR.RetentionSec, got.DVR.RetentionSec)
	assert.Equal(t, want.DVR.SegmentDuration, got.DVR.SegmentDuration)
	assert.Equal(t, want.DVR.StoragePath, got.DVR.StoragePath)
	assert.Equal(t, want.DVR.MaxSizeGB, got.DVR.MaxSizeGB)

	assert.Equal(t, want.CreatedAt.UTC(), got.CreatedAt.UTC())
	assert.Equal(t, want.UpdatedAt.UTC(), got.UpdatedAt.UTC())
}

func TestSQLStreamRepo_FindByCode_NotFound(t *testing.T) {
	ctx := context.Background()
	repo := newSQLStore(t).Streams()

	_, err := repo.FindByCode(ctx, "nonexistent")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrNotFound))
}

func TestSQLStreamRepo_List(t *testing.T) {
	ctx := context.Background()
	repo := newSQLStore(t).Streams()

	s1 := storetest.NewFullStream("stream1")
	s2 := storetest.NewFullStream("stream2")
	s2.Status = domain.StatusStopped

	require.NoError(t, repo.Save(ctx, s1))
	require.NoError(t, repo.Save(ctx, s2))

	all, err := repo.List(ctx, store.StreamFilter{})
	require.NoError(t, err)
	assert.Len(t, all, 2)

	activeStatus := domain.StatusActive
	active, err := repo.List(ctx, store.StreamFilter{Status: &activeStatus})
	require.NoError(t, err)
	assert.Len(t, active, 1)
	assert.Equal(t, domain.StreamCode("stream1"), active[0].Code)
}

func TestSQLStreamRepo_Update(t *testing.T) {
	ctx := context.Background()
	repo := newSQLStore(t).Streams()

	s := storetest.NewFullStream("update_me")
	require.NoError(t, repo.Save(ctx, s))

	s.Name = "Updated Name"
	s.Status = domain.StatusStopped
	require.NoError(t, repo.Save(ctx, s))

	got, err := repo.FindByCode(ctx, "update_me")
	require.NoError(t, err)
	assert.Equal(t, "Updated Name", got.Name)
	assert.Equal(t, domain.StatusStopped, got.Status)
}

func TestSQLStreamRepo_Delete(t *testing.T) {
	ctx := context.Background()
	repo := newSQLStore(t).Streams()

	s := storetest.NewFullStream("delete_me")
	require.NoError(t, repo.Save(ctx, s))
	require.NoError(t, repo.Delete(ctx, "delete_me"))

	_, err := repo.FindByCode(ctx, "delete_me")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrNotFound))
}

// --- RecordingRepository ---

func TestSQLRecordingRepo_SaveAndFindByID(t *testing.T) {
	ctx := context.Background()
	repo := newSQLStore(t).Recordings()

	want := storetest.NewFullRecording("rec1", "stream1")
	require.NoError(t, repo.Save(ctx, want))

	got, err := repo.FindByID(ctx, "rec1")
	require.NoError(t, err)

	assert.Equal(t, want.ID, got.ID)
	assert.Equal(t, want.StreamCode, got.StreamCode)
	assert.Equal(t, want.Status, got.Status)
	assert.Equal(t, want.SegmentDir, got.SegmentDir)
	assert.Equal(t, want.StartedAt.UTC(), got.StartedAt.UTC())
	require.NotNil(t, got.StoppedAt)
	assert.Equal(t, want.StoppedAt.UTC(), got.StoppedAt.UTC())
}

func TestSQLRecordingRepo_FindByID_NotFound(t *testing.T) {
	ctx := context.Background()
	repo := newSQLStore(t).Recordings()

	_, err := repo.FindByID(ctx, "noexist")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrNotFound))
}

func TestSQLRecordingRepo_ListByStream(t *testing.T) {
	ctx := context.Background()
	repo := newSQLStore(t).Recordings()

	r1 := storetest.NewFullRecording("recA", "stream_alpha")
	r2 := storetest.NewFullRecording("recB", "stream_alpha")
	r3 := storetest.NewFullRecording("recC", "stream_beta")

	require.NoError(t, repo.Save(ctx, r1))
	require.NoError(t, repo.Save(ctx, r2))
	require.NoError(t, repo.Save(ctx, r3))

	list, err := repo.ListByStream(ctx, "stream_alpha")
	require.NoError(t, err)
	assert.Len(t, list, 2)

	other, err := repo.ListByStream(ctx, "stream_beta")
	require.NoError(t, err)
	assert.Len(t, other, 1)
	assert.Equal(t, domain.RecordingID("recC"), other[0].ID)
}

func TestSQLRecordingRepo_Delete(t *testing.T) {
	ctx := context.Background()
	repo := newSQLStore(t).Recordings()

	r := storetest.NewFullRecording("del_rec", "stream1")
	require.NoError(t, repo.Save(ctx, r))
	require.NoError(t, repo.Delete(ctx, "del_rec"))

	_, err := repo.FindByID(ctx, "del_rec")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrNotFound))
}

// --- HookRepository ---

func TestSQLHookRepo_SaveAndFindByID(t *testing.T) {
	ctx := context.Background()
	repo := newSQLStore(t).Hooks()

	want := storetest.NewFullHook("hook1")
	require.NoError(t, repo.Save(ctx, want))

	got, err := repo.FindByID(ctx, "hook1")
	require.NoError(t, err)

	assert.Equal(t, want.ID, got.ID)
	assert.Equal(t, want.Name, got.Name)
	assert.Equal(t, want.Type, got.Type)
	assert.Equal(t, want.Target, got.Target)
	assert.Equal(t, want.Secret, got.Secret)
	assert.Equal(t, want.EventTypes, got.EventTypes)
	assert.Equal(t, want.Enabled, got.Enabled)
	assert.Equal(t, want.MaxRetries, got.MaxRetries)
	assert.Equal(t, want.TimeoutSec, got.TimeoutSec)
}

func TestSQLHookRepo_FindByID_NotFound(t *testing.T) {
	ctx := context.Background()
	repo := newSQLStore(t).Hooks()

	_, err := repo.FindByID(ctx, "ghost")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrNotFound))
}

func TestSQLHookRepo_List(t *testing.T) {
	ctx := context.Background()
	repo := newSQLStore(t).Hooks()

	require.NoError(t, repo.Save(ctx, storetest.NewFullHook("h1")))
	require.NoError(t, repo.Save(ctx, storetest.NewFullHook("h2")))

	list, err := repo.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

func TestSQLHookRepo_Delete(t *testing.T) {
	ctx := context.Background()
	repo := newSQLStore(t).Hooks()

	require.NoError(t, repo.Save(ctx, storetest.NewFullHook("del_hook")))
	require.NoError(t, repo.Delete(ctx, "del_hook"))

	_, err := repo.FindByID(ctx, "del_hook")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrNotFound))
}
