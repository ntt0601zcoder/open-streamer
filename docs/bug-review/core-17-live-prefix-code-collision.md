# Stream code hoặc template prefix bắt đầu bằng live/ không truy cập được đúng qua RTMP
- **Mức độ:** Thấp
- **Độ tin cậy:** Trung bình
- **Module liên quan:** domain, ingestor/push, autopublish
- **Vị trí:** internal/domain/stream.go:16,23; internal/domain/template.go:176; internal/ingestor/push/rtmp_server.go:66-88,303-316

## Hiện tượng
Stream code `live/cam1` hợp lệ khi lưu, nhưng push tới `rtmp://host/live/cam1` bị từ chối hoặc đi vào stream `cam1`. Template prefix `live` không bao giờ khớp push RTMP dạng `live/foo/bar`.

## Điều kiện kích hoạt
Tạo stream có code bắt đầu bằng `live/`, hoặc template có `prefixes: ["live"]`, rồi push RTMP.

## Phân tích
`rtmpRouteKey` luôn cắt một tiền tố `live/`:
```go
combined = strings.TrimPrefix(combined, "live/")
```
Key thành `cam1` và không khớp slot `live/cam1`. Với auto-publish, `foo/bar` được truyền vào resolver và `PrefixMatches("live", "foo/bar")` là false.

## Vì sao đây là lỗi
`ValidateStreamCode` và `ValidatePrefix` không chặn dạng này, trong khi tài liệu ARCHITECTURE dùng chính ví dụ prefix `live` khớp `live/foo/bar`. USER_GUIDE nói multi-segment dùng nguyên code. Hai stream `live/cam1` và `cam1` còn có thể tranh nhau cùng URL.

## Hướng sửa
Từ chối `live` làm segment đầu ở code và prefix, hoặc thử khớp đường dẫn gốc trước khi cắt `live/`.

## Cách xác minh
Unit test: `rtmpRouteKey("live","cam1")` cho ra `cam1` trong khi registry có key `live/cam1`; `PrefixMatches("live", rtmpRouteKey("live","foo/bar"))` là false.
