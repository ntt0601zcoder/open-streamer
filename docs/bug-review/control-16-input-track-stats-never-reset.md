# Thống kê track của input không bao giờ reset, UI hiện bitrate và độ phân giải cũ
- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** manager, api/handler
- **Vị trí:** internal/manager/tracks.go:44-68, 128-134, 176-182; internal/manager/service.go:1201-1210, 941, 543-548; internal/api/handler/stream.go:183-186

## Hiện tượng
- Input đã chết vẫn hiển thị `bitrate_kbps` là giá trị cuối cùng, và `media.input_bitrate_kbps` vẫn khác 0.
- Sau khi đổi URL của input sang nguồn khác độ phân giải cùng codec, panel vẫn hiện độ phân giải cũ.

## Điều kiện kích hoạt
Nguồn ngừng gửi packet, hoặc `PUT` đổi URL của input giữ nguyên priority, hoặc failover.

## Phân tích
`bitrateBps` chỉ được tính lại trong `recordBytes`, tức chỉ khi có packet mới, nên khi không có packet nó giữ nguyên. Hàm dọn dẹp có comment nêu đúng mục đích nhưng không có nơi gọi (grep `.reset()` chỉ thấy trong `pull/file.go`):
```go
// reset clears all per-track counters. Called on input switch / unregister so
// stale bitrate readings from a dead source don't keep showing on the UI.
func (s *inputTrackStats) reset() {
```
`spsParsed` cũng chặn việc parse lại SPS sau lần đầu.

## Vì sao đây là lỗi
Comment mô tả hành vi không tồn tại trong code. `commitSwitch`, `UpdateInputs` và nhánh mark Degraded đều không xoá stats.

## Hướng sửa
Gọi `h.tracks.reset()` trong `commitSwitch` cho input cũ, khi `UpdateInputs` đổi cấu hình input, và khi input chuyển Degraded. Có thể cho snapshot trả 0 khi window quá cũ.

## Cách xác minh
Test: observe packet cho đủ một window, gọi `ReportInputError`, kiểm tra `BitrateKbps` trong `RuntimeStatus` về 0. Hiện tại giữ giá trị cũ.
