# Composition time âm của RTMP bị đọc thành số dương cực lớn
- **Mức độ:** Thấp
- **Độ tin cậy:** Trung bình
- **Module liên quan:** ingestor/pull, ingestor/push, timeline, thư viện lal
- **Vị trí:** internal/ingestor/pull/rtmp_msg_converter.go:135-143; lal v0.37.4 pkg/base/t_rtmp.go:265-275; internal/timeline/normaliser.go:393-397, 932-937

## Hiện tượng
Encoder gửi composition time âm dù chỉ một frame sẽ có PTS của frame đó lệch thêm khoảng 4,66 giờ so với DTS. Player giữ hoặc bỏ frame, hoặc segment có thời lượng sai.

## Điều kiện kích hoạt
Video tag RTMP có trường CompositionTime âm, ví dụ `-1` (`0xFFFFFF`). FLV định nghĩa trường này là SI24.

## Phân tích
lal đọc số không dấu:
```go
return bele.BeUint24(msg.Payload[5:])
```
Converter cộng trực tiếp:
```go
pts := dts + uint64(msg.Cts())
```
`0xFFFFFF` thành `dts + 16777215` ms. Normaliser có `if cto < 0 { cto = 0 }` nhưng ở đây `cto` là số dương lớn, nên không được chặn và `outPts = outDts + cto`.

## Vì sao đây là lỗi
`buildFLVAvcTag` ở chiều ra đã biết trường này có dấu và clamp giá trị âm, chiều vào thì không. Độ tin cậy Trung bình vì chưa xác định encoder cụ thể nào phát CTS âm trong thực tế; đường code thì rõ.

## Hướng sửa
Sign-extend 24 bit trong converter và clamp `pts >= dts` hoặc tuỳ chính sách (ví dụ `cts := int32(raw<<8) >> 8`).

## Cách xác minh
Unit test `Convert` với `RtmpMsg` có CTS `0xFFFFFF`, kiểm tra `PTSms`.
