# Sau reconnect, joint seed đưa output DTS về 0 rồi nhảy về wallclock trong cùng session
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** timeline, ingestor/worker, ingestor/tsnorm, publisher/hls
- **Vị trí:** internal/timeline/normaliser.go:291-302,694-722,783-804,510; internal/ingestor/worker.go:88,227; internal/publisher/hls.go:559,681-682

## Hiện tượng
Mỗi lần pull worker reconnect, sau khi stream đã chạy quá 2 giây, các packet đầu của session mới có PTS gần 0, rồi PTS nhảy lên giá trị wallclock (có thể hàng giờ). Trên HLS đường AV, segment đầu sau reconnect có EXTINF bằng toàn bộ thời gian chạy của worker (ví dụ 3600 giây). Trên đường raw-TS, trong một segment có bước nhảy PTS mà không có discontinuity. Player có thể đứng hoặc nhảy live edge.

## Điều kiện kích hoạt
1. Một Normaliser sống qua các lần reconnect trong cùng worker (`worker.go:88`).
2. Nguồn RTSP/RTMP pull hoặc raw-TS rớt kết nối, worker mở lại và gọi `OnSession`.
3. Cấu hình production có `SeedHoldTimeoutMs=1000`.

Stream chỉ có một track đi qua `timeoutDrainPendingLocked` và bị tương tự.

## Phân tích
`OnSession` xóa track nhưng giữ `wallOrigin`, với hợp đồng "a reconnect's next packet anchors at elapsed since the worker started". Tuy nhiên `completeJointSeedLocked` không dùng `actualNowMs` để anchor:
```go
var currentAnchor, pendingAnchor int64  // 0 và |delta|
pendingTrack.lastOutputDts = pendingAnchor
```
`timeoutDrainPendingLocked` cũng đặt `outputAnchor = 0`, với comment "since wallOrigin was set at the first pending packet's arrival", điều này chỉ đúng ở session đầu.

Packet kế tiếp có `expected` nhỏ, `drift` khoảng `-T`, nên `absInt64(drift) > JumpThresholdMs` kích hoạt re-anchor lên wallclock. Kết quả trong một session: các packet held và packet hiện tại ở DTS 0..delta, các packet sau ở DTS T.

HLS gọi `flushLocked` với `lastMediaPTS - segStartMediaPTS` ≈ T, nên EXTINF sai.

## Vì sao đây là lỗi
`SessionStart` chỉ gắn vào packet đầu, nên bước nhảy xảy ra sau marker và consumer không reset được. `TestOnSessionResetsPerTrackButPreservesWallclock` dùng `DefaultConfig` không bật hold, và `TestJointSeedOnSessionClearsPending` chỉ đếm số packet, nên cấu hình production không được kiểm tra.

## Hướng sửa
Anchor joint seed theo mốc `base = max(actualNowMs, last output của session trước)`: earlier track tại `base`, later track tại `base + |delta|`. Timeout drain cũng vậy. Cần giữ lại floor đơn điệu qua `OnSession`.

## Cách xác minh
Unit test: `New(jointSeedConfig(1000))`, seed và chạy tới T=60 giây, gọi `OnSession`, Apply V rồi A rồi V kế tiếp. Kiểm tra DTS của cả ba đều xấp xỉ 60000 và không có `HardReanchored`.
