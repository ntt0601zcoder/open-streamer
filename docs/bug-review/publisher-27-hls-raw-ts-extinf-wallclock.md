# EXTINF của HLS raw-TS tính theo thời gian thực, không khớp nội dung khi nguồn đến theo đợt
- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** publisher (HLS)
- **Vị trí:** internal/publisher/hls.go:598-602, 635-657, 680-683; internal/publisher/ts_keyframe_scanner.go:148

## Hiện tượng
Với HLS pull hoặc nguồn TS đến theo đợt, EXTINF không khớp thời lượng media trong segment. Player có thể tính sai buffer, seek lệch, hoặc nhảy live edge.

## Điều kiện kích hoạt
Nguồn raw-TS (`Packet.TS`, không có `Packet.AV`) đến theo đợt nhiều GOP, ví dụ chunk 6 giây chứa IDR tại 0, 2, 4 giây, `segDur=2s`.

## Phân tích
Raw-TS không có `hasMediaStart`, nên `dur = time.Since(p.segStart).Seconds()`. Scanner chỉ giữ IDR mới nhất (`LastIDROffset`). Ví dụ:
- t=0: chunk 0-6 giây đến, segStart=0.
- t=2: tick đủ `segDur`, IDR mới nhất là 4 giây, flush media 0-4 giây với `EXTINF=2`.

Sau đó `segStart=time.Now()` tại t=2 trong khi phần carry bắt đầu ở media 4 giây. Khi các chunk có độ dài khác nhau, EXTINF của một segment phản ánh chunk trước đó thay vì nội dung của nó.

## Vì sao đây là lỗi
Comment trong `flushLocked` đã nói wallclock làm EXTINF lệch với đồng hồ player, và ưu tiên PTS khi có. Đường raw-TS không có thay thế. Với nguồn đều thì sai số nhỏ, nên chỉ thấy ở nguồn theo đợt.

## Hướng sửa
Scanner đọc PTS từ PES video tại điểm cắt và tính EXTINF từ PTS giữa hai IDR; chọn IDR đầu tiên qua `segDur` như DASH.

## Cách xác minh
Test feed TS có PTS đã biết theo đợt, không chờ thực: tổng EXTINF phải khớp tổng PTS. Thực tế: HLS pull nguồn 6 giây, so `EXTINF` với `ffprobe` từng segment.
