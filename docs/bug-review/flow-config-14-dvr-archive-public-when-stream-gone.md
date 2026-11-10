# Bản ghi DVR thành công khai khi stream runtime kết thúc hoặc stream bị xoá

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** cmd/server, internal/mediaauth, internal/api, internal/dvr/blob, internal/autopublish, internal/store
- **Vị trí:** cmd/server/main.go:367-381, internal/mediaauth/mediaauth.go:143-149, internal/api/dispatch.go:178-207, internal/api/handler/blob_timeshift.go:68-74, internal/dvr/blob/service.go:117-124, internal/api/handler/stream.go:724-725, docs/ARCHITECTURE.md:1059

## Hiện tượng
Stream có `playback_policy` yêu cầu token và có DVR. Sau khi stream auto-publish hết hạn, hoặc stream cấu hình bị xoá, bất kỳ ai cũng tải được bản ghi qua `/<code>/index.m3u8?from=...` mà không cần token.

## Điều kiện kích hoạt
1. Template có prefix, `playback_policy`, `dvr.enabled: true`, input `publish://`.
2. Encoder đẩy một lúc rồi ngắt, chờ reaper dừng stream sau 30 giây.
3. Gọi `/<code>/index.m3u8?ago=60` không có token.

## Phân tích
`resolvePolicy` có hai tầng: publisher khi đang chạy, store khi đã dừng. Stream runtime không bao giờ có trong store, và stream đã xoá cũng không còn:

```go
s, err := streamRepo.FindByCode(context.Background(), code)
if err != nil {
    return ""
}
```

`Authorize` coi `""` là công khai:

```go
if code == "" {
    return allow() // no policy → public
}
```

Trong khi đó `StartRecording` lưu `Recording` row và catalog trên đĩa. `IsBlob` tìm thấy row nên `ServeTimeshift` phục vụ bình thường. `StreamHandler.Delete` không đụng tới Recording.

Việc này cũng có nghĩa stream runtime để lại bản ghi trong store (`recordings`), khác với bảng invariant "Runtime streams never reach the store".

## Vì sao đây là lỗi
Comment của `wireMediaAuth` nói fallback tồn tại để "policy binding survives" với stream đã dừng. Lỗi store (kể cả không phải NotFound) cũng dẫn tới fail-open, trong khi policy không xác định lại fail-closed.

## Hướng sửa
Lưu `PlaybackPolicy` trong Recording row hoặc catalog khi bắt đầu ghi và fallback vào đó. Nếu không xác định được stream nhưng có bản ghi thì từ chối. Xoá hoặc đánh dấu bản ghi khi xoá stream.

## Cách xác minh
Test tích hợp `resolvePolicy`: code có Recording nhưng không có trong store và publisher phải bị từ chối. Thử thực tế theo các bước trên.
