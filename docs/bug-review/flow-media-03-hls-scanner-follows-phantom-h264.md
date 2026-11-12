# Scanner HLS theo PID H.264 ảo nên nguồn raw-TS HEVC không cắt được tại IDR
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/tsnorm, publisher (HLS)
- **Vị trí:** internal/ingestor/tsnorm/tsnorm.go:221-229, 473-481; internal/publisher/ts_keyframe_scanner.go:239-253; internal/publisher/hls.go:598-624, 635-645

## Hiện tượng
Nguồn UDP/SRT/HLS/HTTP-TS HEVC, không transcode: segment HLS dài bằng 4 lần `segSec` (8 giây với mặc định 2) và không bắt đầu bằng keyframe. Player chỉ decode được khi gặp IRAP bên trong segment.

Ghi chú: core-06 đã mô tả PID ảo và PCR PID. File này ghi hậu quả riêng ở scanner HLS, vì một bản sửa chỉ đổi `SetPCRPID` sẽ không khắc phục được.

## Điều kiện kích hoạt
Nguồn raw-TS có video H.265, output HLS, không có transcoder.

## Phân tích
`buildMuxer` đăng ký H.264 trên `0x100` trước, H.265 được thêm sau trên `0x102`. Thứ tự PMT vì vậy là H.264, AAC, H.265.

`parsePMT` dừng ở video đầu tiên:
```go
case 0x1B: // H.264
    s.videoPID = esPID
    s.videoCodec = domain.AVCodecH264
    return
```
Scanner theo dõi PID `0x100`, nơi không có packet nào, nên `LastIDROffset` luôn là -1 và `tryFlushAtIDRLocked` luôn trả false.

Chỉ còn đường force-flush ở `maxDur`:
```go
if elapsed >= maxDur {
    p.flushLocked()
```
Nhánh `discardUntilIDR` chỉ chạy khi `scanner == nil`, nên segment sau tiếp tục bắt đầu giữa GOP.

## Vì sao đây là lỗi
Scanner có code riêng cho HEVC (`scanForH265IRAP`) nhưng không bao giờ tới được, vì giả định "video đầu tiên trong PMT là video thật" không còn đúng với output của tsnorm.

## Hướng sửa
Đăng ký stream theo codec thực tế (hướng của core-06), để PMT chỉ có một video. Nếu giữ PID cố định thì `parsePMT` phải ưu tiên PID có dữ liệu.

## Cách xác minh
Đưa TS HEVC qua `tsnorm.Process` rồi vào `tsKeyframeScanner.Feed`. Kiểm tra `videoCodec == H265` và `LastIDROffset >= 0`. Hiện tại `videoCodec` là H264 và offset là -1.
