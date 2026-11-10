# DASH giữ init segment cũ khi failover sang nguồn khác tham số codec
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher/dash
- **Vị trí:** internal/publisher/dash/packager.go:361, 380-383, 459-471, 605-616

## Hiện tượng
Sau failover sang nguồn khác độ phân giải, profile, sample rate hoặc codec, DASH vỡ hình, mất tiếng hoặc player dừng. MPD vẫn khai width/height/codec cũ. HLS cùng stream có thể vẫn phát vì có DISCONTINUITY.

## Điều kiện kích hoạt
Stream không transcode (hoặc copy) có nhiều input khác thông số, ví dụ primary 1080p H.264/48 kHz và backup 720p/44,1 kHz hoặc H.265.

## Phân tích
Init chỉ được tạo khi còn nil, và boundary cố ý giữ nguyên:
```go
if p.videoInit == nil && !p.videoPSGivenUp { ... }
if p.audioInit == nil { ... }
```
`isHEVC` chỉ chuyển thành true, không bao giờ về false. Comment boundary ghi "Init segments + AST + segment counters ... survive". Sau boundary:
- SPS/PPS mới không vào `init_v.mp4`.
- Sample rate mới vẫn dùng timescale cũ, trong khi `len(frames) x 1024` tính theo tick của timescale cũ nên sai thời lượng.
- H.264 sang H.265: frame HEVC được đưa vào fragment với init `avc1`.

## Vì sao đây là lỗi
Nguồn mới có thể khác tham số; đó là lý do RTMP push tự restart ở session boundary. DASH cần init hoặc Period mới. Không có guard so sánh SPS/ASC ở boundary.

## Hướng sửa
So sánh tham số ở IDR/ADTS đầu sau boundary. Nếu khác thì tạo init có version mới và mở Period mới (hoặc Representation mới) với số segment liên tục.

## Cách xác minh
Test: boundary rồi feed IDR có SPS khác, kiểm tra init mới và MPD đổi kích thước. Thực tế: failover giữa hai nguồn 1080p và 720p, xem dash.js và `ffprobe`.
