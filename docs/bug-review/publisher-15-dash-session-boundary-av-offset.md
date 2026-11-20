# Mỗi session boundary tích lũy thêm độ lệch A/V trong DASH do không ghép cặp lại
- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** publisher/dash
- **Vị trí:** internal/publisher/dash/packager.go:600-616, 711-714; internal/publisher/dash/segmenter.go:291-305

## Hiện tượng
Sau một hoặc nhiều lần failover, tiếng lệch hình ngày càng nhiều. HLS trên cùng stream không bị.

## Điều kiện kích hoạt
Session mới có frame audio đầu và frame video đầu khác PTS, ví dụ audio tới trước IDR 500 ms khi nguồn mới vào giữa GOP.

## Phân tích
`pairingTruncated` cố ý không reset, và `onSessionBoundary` chỉ xóa queue. Sau boundary, audio từ PTS `t-500` và video từ PTS `t` được cắt theo số lượng:
```go
totalSamples := vDurMs*audioSR/1000 + s.audioFrameResidualSamples
targetA := int(totalSamples / 1024)
```
Cả hai track đều đi tiếp từ tfdt cuối của chính mình (`videoTfdtForSegment`, `audioTfdtForSegment`). Sample audio đầu tại tfdt_A ứng với `t-500`, sample video đầu tại tfdt_V ứng với `t`, nên lệch 500 ms được ghi cứng. Boundary tiếp theo cộng thêm độ lệch mới.

## Vì sao đây là lỗi
Comment cho rằng các track "join their existing timelines", nhưng PTS của phiên mới không dùng để định vị tfdt, nên hai đầu track không còn cùng thời điểm. Lý do không reset latch là tránh truncate lặp, không phải vì không cần căn chỉnh.

## Hướng sửa
Sau boundary, căn V và A về cùng PTS đúng một lần cho phiên mới, chờ IDR rồi bỏ audio trước IDR. Có thể gộp với hướng sửa của publisher-14.

## Cách xác minh
Unit test: sau boundary, feed audio sớm hơn IDR 500 ms và kiểm tra chênh lệch tfdt/PTS giữa A và V ở segment đầu phiên mới là dưới một frame.
