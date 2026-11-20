# Một request HTTP có path không phải UTF-8 làm sập cả server sau khi hết idle

- **Mức độ:** Nghiêm trọng
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/sessions, internal/api, prometheus client
- **Vị trí:** internal/sessions/httpmw.go:31-55, internal/sessions/httpmw.go:75-86, internal/sessions/tracker.go:297-305, internal/sessions/tracker.go:577-587, internal/sessions/tracker.go:591-605, internal/sessions/reaper.go:21-35, internal/sessions/reaper.go:87-89, internal/runtime/manager.go:454-459

## Hiện tượng
Khoảng 30 giây sau một request không cần xác thực, process `open-streamer` chết vì panic trong goroutine reaper. Mọi stream và mọi viewer rớt cùng lúc.

## Điều kiện kích hoạt
1. `sessions.enabled=true`, metrics được wire (mặc định trong `main`).
2. Client gửi `GET /%FF/a.ts`. Bất kỳ phần đuôi nào trong `.ts/.m3u8/.mpd/.m4s/.mp4` đều được.
3. Chờ hết `idle_timeout_sec` (mặc định 30s).

## Phân tích
`r.URL.Path` đã giải mã percent nên chứa byte `0xFF`. Middleware lấy stream code thẳng từ path, không validate:
```go
code := streamCodeFromPath(r.URL.Path)
...
next.ServeHTTP(cw, r)   // dispatchMedia trả 404
...
t.TrackHTTP(r.Context(), hit)
```
`TrackHTTP` đưa session vào map rồi mới gọi metric:
```go
s.sessions[id] = sess
s.mu.Unlock()
s.observeOpened(sess)   // GaugeVec.With(labels)
```
`With` panic khi giá trị label không phải UTF-8 hợp lệ (`validateValuesInLabels`, `GaugeVec.With`: `if err != nil { panic(err) }`). Ở lượt này chi `Recoverer` chặn panic, nhưng session đã nằm trong map.

Khi hết idle, `reapOnce` gọi `closeByIDCtx`, rồi `observeClosed` gọi lại `s.m.active.With(streamLabels)` và panic lần nữa, lần này trên goroutine của `runReaper`.

## Vì sao đây là lỗi
`ValidateStreamCode` chỉ chạy trong `dispatchMedia`, không bảo vệ giá trị middleware dùng. Cả repo không có `recover()` trên đường reaper, và `startService` chạy `fn(ctx)` trong goroutine trần, nên panic làm chết process. Route media không yêu cầu auth.

## Hướng sửa
Chỉ track khi `domain.ValidateStreamCode(code)` đạt, tốt nhất chỉ khi stream tồn tại. Chuyển `observeOpened/observeClosed` sang `GetMetricWith` có xử lý lỗi, hoặc gọi `observeOpened` trước khi insert.

## Cách xác minh
Unit test với `NewServiceForTesting` gắn `metricsHooks` thật: gọi `TrackHTTP` với `StreamCode: "\xff"` và kiểm tra panic, rồi gọi `reapOnce` với đồng hồ giả. Chỉ thử end-to-end trên môi trường riêng, không thử trên máy đang chạy stream.
