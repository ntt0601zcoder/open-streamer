# Nguồn H.265 raw-TS có PMT sai: PID H.264 ảo, PCR sai PID và không có PSI trước IDR
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/tsnorm, publisher, thư viện astits
- **Vị trí:** internal/ingestor/tsnorm/tsnorm.go:214-230, 472-487, 517-523; go-astits v1.15.0 muxer.go:173-177

## Hiện tượng
Stream H.265 qua UDP/HLS/SRT/file có PMT hiển thị luồng H.264 không có dữ liệu cùng với luồng HEVC, PCR không nằm trên PID được khai báo. Player vào giữa stream phải chờ PSI định kỳ thay vì có ngay trước IDR, làm mất GOP đầu ở các segment bắt đầu bằng IDR. Audio-only cũng có PCR_PID trỏ tới video không tồn tại.

## Điều kiện kích hoạt
Bất kỳ nguồn raw-TS H.265 nào, hoặc nguồn chỉ có audio.

## Phân tích
Muxer luôn đăng ký H.264 và lấy nó làm PCR PID:
```go
_ = n.muxer.AddElementaryStream(astits.PMTElementaryStream{ElementaryPID: videoPID, StreamType: astits.StreamTypeH264Video})
...
n.muxer.SetPCRPID(videoPID)
```
H.265 được thêm lazy trên PID `0x102`, PCR PID không đổi. astits chỉ ép bảng khi PID trùng PCR PID:
```go
forceTables := d.AdaptationField != nil &&
    d.AdaptationField.RandomAccessIndicator &&
    d.PID == m.pmt.PCRPID
```
Vì `d.PID = 0x102`, IDR H.265 không bao giờ có PAT/PMT đi kèm; bảng chỉ được phát theo chu kỳ 40 PES. Trong `writeMuxed`, `HasPCR: true` ghi PCR lên `0x102`, trong khi PMT khai báo `PCR_PID = 0x100` không có gói nào.

## Vì sao đây là lỗi
Header tsnorm nêu mục tiêu là PSI đứng trước mỗi IDR để tránh lỗi "non-existing PPS", và comment trên const nói tránh "phantom HEVC" gây lỗi cho demuxer nghiêm ngặt. Nhánh H.265 tạo đúng lớp lỗi đó theo chiều ngược lại. HLS trên đường raw-TS cắt tại IDR nên không tự chèn PSI.

## Hướng sửa
Khởi tạo muxer lazy theo PMT nguồn, chỉ thêm các PID thực sự có. Đặt PCR PID theo video thực, audio-only thì dùng audio PID và ghi PCR trên audio. Có thể gọi `WriteTables()` trước IDR khi không phải PCR PID.

## Cách xác minh
Unit test: `Process` TS H.265, parse output kiểm tra PMT và vị trí PAT so với PES IDR. Thủ công: `ffprobe -show_streams -show_programs` trên HLS của một nguồn HEVC.
