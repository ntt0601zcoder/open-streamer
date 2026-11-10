# DASH thiếu bù composition offset, video có B-frame hiển thị trễ hơn audio
- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** publisher/dash
- **Vị trí:** internal/publisher/dash/fmp4_writer.go:54-56, 184, 193; internal/publisher/dash/packager.go:918-926, 829-847

## Hiện tượng
Luồng có B-frame phát DASH lệch lip-sync cố định, video chậm hơn audio khoảng độ trễ sắp xếp lại (thường 80-120 ms với 2-3 B-frame). Thời điểm trong MPD cũng lệch so với thời điểm trình chiếu thật của segment.

## Điều kiện kích hoạt
Video có B-frame, nên IDR đầu segment có `PTS > DTS`.

## Phân tích
```go
cto := int32((int64(f.PTSms) - int64(f.DTSms)) * 90)
```
Sample đầu có decode time = tfdt, presentation time = `tfdt + cto`. V và A khởi đầu cùng tfdt, trong khi `truncateAtPairingLocked` căn hai track theo PTS. Do đó audio trình chiếu tại tfdt, còn video tại tfdt+cto. Init tạo bằng `CreateEmptyInit`/`AddEmptyTrack` không có edit list, và MPD không có `presentationTimeOffset` trong chế độ live.

## Vì sao đây là lỗi
Định vị theo tfdt là decode time, trong khi quan hệ A/V được căn theo PTS. Không có cơ chế nào bù phần chênh. `S@t` theo DASH phải khớp thời điểm trình chiếu sớm nhất của segment.

## Hướng sửa
Dùng trun version 1 với offset âm để PTS sample đầu trùng tfdt, hoặc đặt edit list/`presentationTimeOffset`, hoặc tính tfdt video theo DTS đã trừ cto đầu tiên.

## Cách xác minh
Dump `moof` của `seg_v_*.m4s` với nguồn `bframes=2` và so PTS đầu với tfdt và tfdt audio. Thử clip đo A/V sync trên dash.js.
