# Safety-net cut của DASH làm audio tụt dần so với video trên nguồn GOP dài
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher/dash
- **Vị trí:** internal/publisher/dash/packager.go:44, 384-389, 771-773; internal/publisher/dash/segmenter.go:161-166, 291-305; internal/publisher/dash/frame_queue.go:94

## Hiện tượng
Trên nguồn GOP dài, DASH mất khoảng hình đều đặn, audio trễ hơn video ngày càng nhiều (tới khoảng 30 giây), và timeline tụt khỏi đồng hồ.

## Điều kiện kích hoạt
GOP lớn hơn `3 x segDur`. Với mặc định `segDur=2s` là GOP trên 6 giây, ví dụ GOP 10 giây.

## Phân tích
Ví dụ GOP 10 giây, safety-net tại 6 giây:
1. Cut không căn IDR xả hết video, rồi `firstIDRSeen=false`.
2. Video từ 6 đến 10 giây bị `handleH264` bỏ hết, trong khi `handleAAC` vẫn nhận audio.
3. Chu kỳ sau, V chỉ có 6 giây nên `targetA` lấy 6 giây audio cũ nhất. Queue audio dư thêm 4 giây.

Mỗi GOP thêm 4 giây lệch A/V, tới trần `maxQueueSpanMs=30s`. Tfdt video cũng chỉ tăng 6 giây mỗi 10 giây thực.

## Vì sao đây là lỗi
HLS đã xử lý tình huống tương tự bằng multiplier 4 và mỗi segment TS giữ PTS thật. DASH cộng dồn tfdt và ghép audio theo số lượng, nên khoảng video bị bỏ không được bù bên audio.

## Hướng sửa
Bỏ audio tương ứng khoảng video đã drop, hoặc không drop video mà giữ nguyên tới IDR kế (chấp nhận segment dài hơn). Nên dùng multiplier như HLS hoặc theo GOP đo được.

## Cách xác minh
Unit test mô phỏng GOP 10 giây, `segDur=2s`, chạy 5 GOP và đo chênh giữa PTS audio đầu segment với PTS video đầu segment.
