# /metrics công khai làm lộ URL push kèm stream key

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/metrics, internal/publisher, internal/api
- **Vị trí:** internal/api/server.go:205-209, internal/metrics/metrics.go:350-353, internal/metrics/metrics.go:431-434, internal/metrics/metrics.go:469-472, internal/publisher/service.go:688-703, internal/publisher/push_rtmp.go:391, internal/publisher/push_rtmp.go:473, internal/publisher/runtime.go:112, internal/publisher/runtime.go:126

## Hiện tượng
Bất kỳ ai truy cập được cổng HTTP đều đọc được `rtmp://a.rtmp.youtube.com/live2/<STREAM_KEY>` hoặc `rtmp://user:pass@host/...` trong label `dest_url`. Người đó có thể chiếm kênh đích.

## Điều kiện kích hoạt
Stream có `push` đích, gọi `GET /metrics` không xác thực. Bật auth quản trị cũng không thay đổi vì route nằm ngoài nhóm auth.

## Phân tích
```go
r.Handle("/metrics", promhttp.Handler())
```
Label dùng nguyên chuỗi cấu hình:
```go
c := s.m.PublisherPushBytes.WithLabelValues(string(streamID), destURL)
```
với lời gọi `s.pushBytesObserver(streamID, dest.URL)`. `setPushStatus` và counter reconnect cũng dùng `dest.URL`. Không có bước che giấu nào. Comment metric còn khuyến khích dùng "per dest_url" cho dashboard, nên giá trị này cũng lan sang Prometheus/Grafana.

## Vì sao đây là lỗi
Với RTMP push, path chính là credential. Auth của API đã được thêm để giới hạn đúng thông tin này (`GET /streams` nằm sau auth), nhưng metric bỏ qua.

## Hướng sửa
Đặt label là định danh không bí mật: host + chỉ số destination, hoặc hash ngắn của URL. Tốt nhất cũng cho phép đưa `/metrics` vào sau auth hoặc listener riêng.

## Cách xác minh
Test: `pushBytesObserver` với URL có key, thu thập metric bằng `testutil`, kiểm tra chuỗi key xuất hiện. Xem trực tiếp trên môi trường riêng.
