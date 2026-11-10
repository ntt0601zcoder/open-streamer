# Sau restart, mọi đoạn DVR của lần chạy trước không phát được dù status vẫn báo có

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/dvr/blob, internal/api/handler
- **Vị trí:** internal/dvr/blob/service.go:90-99, internal/dvr/blob/service.go:111, internal/dvr/blob/service.go:206-218, internal/dvr/blob/service.go:292-298, internal/dvr/blob/writer.go:125-129, internal/dvr/blob/catalog.go:136-147, internal/dvr/blob/reader.go:113-115, internal/dvr/blob/reader.go:172-190, internal/dvr/blob/reader.go:361-367, internal/api/handler/blob_timeshift.go:110-129

## Hiện tượng
Sau bất kỳ restart nào của recording, timeshift về trước mốc đó trả playlist rỗng (200, không có segment) hoặc playlist lẫn hai dòng thời gian. `recording_status.json` vẫn hiển thị `dvr_range` đầy đủ. Từ lúc restart tới frame video đầu tiên, cả archive đều không truy cập được.

## Điều kiện kích hoạt
Recording chạy nhiều giờ, restart stream hoặc server, rồi yêu cầu `?from=<trước restart>&dur=600`.

## Phân tích
Mỗi lần chạy lấy tick từ 0 theo PTS của frame đầu (`originPTSms`). Catalog chỉ có một mốc, và `newCatalog` tạo mốc rỗng được `Save` ngay:
```go
cat := newCatalog(code, profiles, cfg)
...
rec.cat.Save(streamDir)  // RecordingMediaOriginUnixMs == 0
```
Sau `setOrigin`, wall time trước restart cho tick âm và bị kẹp về 0:
```go
t := int64(c.RecordingMediaOriginTicks) + delta
if t < 0 { return 0 }
```
Với window nằm hẳn trước restart, `endTicks == 0` nên `snapVideo` dừng ngay ở phần tử đầu và trả rỗng; `Query` không báo lỗi, handler render playlist không có segment. Window bắc qua restart thì ghép tick lớn của lần cũ với tick nhỏ của lần mới, không có cờ discontinuity. `ReadInit` cũng chỉ lấy init của hour mới nhất, nên fragment cũ có thể đi với init khác.

## Vì sao đây là lỗi
Comment ghi nhận "needs a per-hour reader anchor (follow-up)", nhưng API vẫn quảng cáo khoảng này là khả dụng và trả 200. Với deploy thông thường, archive mất khả năng phát sau mỗi lần restart.

## Hướng sửa
Lưu origin theo từng run hoặc từng hour trong `HourRecord`, và map wall sang tick theo hour; tốt hơn là chọn fragment trực tiếp theo `WallTimeMs` đã có trong record. Đặt discontinuity và init theo từng hour. Trong lúc chưa sửa, `Summary` chỉ nên báo khoảng của run hiện tại.

## Cách xác minh
Unit test: hai run liên tiếp với origin khác nhau trên cùng streamDir, `Query` vào khoảng của run đầu, kiểm tra `Video` rỗng.
