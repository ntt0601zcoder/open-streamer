# Nguyên nhân gốc của log `dash: video OOO push`: so sánh PTS trên queue thứ tự giải mã
- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher/dash
- **Vị trí:** internal/publisher/dash/frame_queue.go:135-153; internal/publisher/dash/packager.go:420-429

## Hiện tượng
Mọi stream có B-frame liên tục sinh cảnh báo `dash: video OOO push (PTSms regressed vs queue tail)`, làm nhiễu log và che các bất thường thật.

## Điều kiện kích hoạt
Video có B-frame, thứ tự giải mã ví dụ PTS `0, 120, 40, 80, 240`.

## Phân tích
Video vào queue theo thứ tự giải mã nhưng phép kiểm tra dùng PTS:
```go
if n := len(q.video); n > 0 && f.PTSms < q.video[n-1].PTSms { ooo = true }
```
Mỗi B-frame có PTS nhỏ hơn frame đứng trước nó nên đều bị tính là OOO. `pushVideoWithDiag` ghi Warn ở lần 1 và mỗi 100 lần.

## Vì sao đây là lỗi
Chính comment của `PushVideo` thừa nhận B-frame là bình thường, nhưng log vẫn ở mức Warn và gọi đó là "upstream Normaliser anomaly". Đây là chẩn đoán sai, không phải lỗi dữ liệu. Không gây sai timeline: `findIDRCutPoint` và `computeVideoSegDurTicks` so giữa các IDR.

## Hướng sửa
So sánh `DTSms` để phát hiện đảo thứ tự thật. Giữ PTS cho phần tính span.

## Cách xác minh
Unit test: dãy decode-order có B-frame với DTS tăng không được báo OOO; DTS giảm thì phải báo.
