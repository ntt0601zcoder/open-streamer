# Joint seed bỏ qua SameTimebaseMaxMs, mixer:// bị lệch A/V cố định theo chênh lệch uptime
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** timeline, ingestor/pull (mixer), ingestor/service
- **Vị trí:** internal/timeline/normaliser.go:687-708,565-581,851-862; internal/ingestor/service.go:686; internal/ingestor/pull/mixer.go:8-10

## Hiện tượng
Stream `mixer://V,A` có video và audio lệch nhau nhiều phút đến nhiều giờ, và lệch này không tự hết. Player không đồng bộ được hoặc đứng hình. Log có dòng `timeline: joint seed` với `source_delta_ms` rất lớn.

## Điều kiện kích hoạt
Hai upstream của mixer khởi động ở các thời điểm khác nhau, cách nhau hơn vài giây. Ví dụ V đã chạy 2 giờ, A mới chạy 5 phút, sau đó tạo hoặc restart stream mixer.

Trigger phụ: session bắt đầu đúng lúc input 33-bit wrap trong cửa sổ hold 1 giây (`inDts` của pending chưa unwrap).

## Phân tích
`mixer.go` ghi rõ "timestamps pass through unchanged", và PTS của mỗi upstream là thời gian tính từ lúc worker upstream đó khởi động. Worker của mixer dùng cấu hình production có `SeedHoldTimeoutMs: 1000`, nên đi vào:
```go
delta := inDts - pendingFirst.inDts
...
case delta > 0:
    currentAnchor = delta
```
Không có kiểm tra `SameTimebaseMaxMs`. Vậy video được anchor tại +6.900.000 ms, audio tại 0.

Sau đó không có nhánh nào kéo lại. `drift` so với `max(actualNow, lastOutputDts)` nên luôn xấp xỉ một frame. Ngay cả khi có drift, `consistentWithSeedOffsetLocked` còn chủ động chặn re-anchor vì offset này đã được ghi nhận là `seedOffsetMs`.

## Vì sao đây là lỗi
`seedTrackLocked` có guard đúng (`absInt64(sourceDelta) <= SameTimebaseMaxMs`, nếu không thì `wallclock_independent_timebase`), và doc của field nói rõ trên ngưỡng thì hai clock được coi là độc lập. Đường joint seed, là đường production, lại không áp dụng. Giới hạn đã biết trong ARCHITECTURE chỉ nói về drift tích lũy, không phải lệch toàn bộ uptime ngay từ đầu.

`abr_mixer` không bị vì cấu hình của nó không bật hold.

## Hướng sửa
Trong `completeJointSeedLocked`, nếu `|delta| > SameTimebaseMaxMs` thì không dùng delta: anchor cả hai track theo wallclock và không set `seedOffsetSet`. Xử lý cùng lúc với core-03.

## Cách xác minh
Unit test với `jointSeedConfig(1000)`: `Apply(aPkt(300000,...))` rồi `Apply(vPkt(7200000,...))`, kiểm tra hai output DTS chênh nhau không quá `SameTimebaseMaxMs`. Hiện tại chênh 6.900.000.
