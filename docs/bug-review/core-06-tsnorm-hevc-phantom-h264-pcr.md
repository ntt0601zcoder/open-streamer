# Output tsnorm với HEVC hoặc chỉ audio có PID H.264 ảo và PCR PID không có dữ liệu
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/tsnorm, publisher
- **Vị trí:** internal/ingestor/tsnorm/tsnorm.go:221-229,472-487,516-523; /home/ntt/go/pkg/mod/github.com/asticode/go-astits@v1.15.0/muxer.go (WriteData: forceTables)

## Hiện tượng
Nguồn raw-TS HEVC: PMT liệt kê hai video stream (H.264 trên 0x100 không có dữ liệu và HEVC trên 0x102). PCR PID trỏ vào PID rỗng. Player chọn PID video đầu tiên sẽ không có hình. PAT/PMT không được phát lại trước IDR, nên segment HLS có thể không bắt đầu bằng PSI.

Nguồn chỉ có audio: PMT có PID H.264 ảo và không có PCR nào.

## Điều kiện kích hoạt
UDP/HLS/SRT/file với video H.265, hoặc nguồn AAC không có video.

## Phân tích
`buildMuxer` luôn đăng ký H.264 và `SetPCRPID(videoPID)`. Nhánh H.265 thêm stream nhưng không đổi PCR PID, trong khi PES của HEVC lại mang field PCR trên `h265PID`.

astits chỉ force table khi `d.PID == m.pmt.PCRPID`, nên IDR trên 0x102 không bao giờ kích hoạt PAT/PMT; PSI chỉ còn theo chu kỳ 40 PES. Audio-only: không có PES nào mang PCR.

## Vì sao đây là lỗi
Comment trong file lo tránh "phantom HEVC" cho nguồn H.264, nhưng tạo ra phantom H.264 cho nguồn HEVC. Cơ chế PSI-trước-IDR mà package này tồn tại để bảo đảm không có tác dụng với HEVC.

## Hướng sửa
Đăng ký stream lazy theo codec thực tế của packet đầu tiên, hoặc khi gặp HEVC thì gỡ PID 0x100 và `SetPCRPID(h265PID)`. Audio-only: đặt PCR PID là audio và phát PCR trên audio.

## Cách xác minh
Đưa TS HEVC qua `tsnorm.Process`, parse PMT: kiểm tra chỉ có một video stream và `PCR_PID` trùng PID video có dữ liệu; kiểm tra PAT/PMT đứng ngay trước mỗi IDR.
