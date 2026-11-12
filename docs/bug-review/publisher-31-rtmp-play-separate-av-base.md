# RTMP play dùng hai mốc thời gian riêng cho audio và video, tạo lệch A/V cố định
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (RTMP play)
- **Vị trí:** internal/publisher/serve_rtmp.go:280-314; internal/publisher/push_rtmp.go:330-345

## Hiện tượng
Khi xem qua RTMP, tiếng và hình lệch cố định từ đầu đến cuối phiên, thay đổi mỗi lần client kết nối lại.

## Điều kiện kích hoạt
Frame audio đầu sau keyframe đầu tiên có DTS khác DTS keyframe. Ví dụ output transcoder với audio đi trước video (comment `holdAudioForVideo` nêu rõ), hoặc TS có audio interleave lệch vài trăm ms.

## Phân tích
```go
relDTS := uint32(dts - ps.baseDTS)    // video
...
if !ps.baseADTSSet { ps.baseADTS = dts; ps.baseADTSSet = true }
relDTS := uint32(dts - ps.baseADTS)   // audio
```
Hai track đều bắt đầu từ 0 dù frame đầu của chúng không cùng thời điểm. Nếu audio đầu có DTS = V0 + 300 ms thì audio bị dời sớm 300 ms so với video trong suốt phiên.

## Vì sao đây là lỗi
RTMP có một timeline duy nhất cho cả phiên. RTMP push dùng một `baseDTS` chung cho cả hai track. Normaliser đã giữ quan hệ A/V, và phần này làm mất quan hệ đó.

## Hướng sửa
Dùng chung `baseDTS` của keyframe đầu tiên cho audio, loại audio có `dts < baseDTS` để tránh underflow uint32.

## Cách xác minh
Unit test `onTSFrame`: video DTS 1000, audio DTS 1300; `writeFrame` audio phải nhận timestamp 300.
