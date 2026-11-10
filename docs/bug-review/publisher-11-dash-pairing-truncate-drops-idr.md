# DASH cắt mất IDR đầu tiên khi ghép cặp V/A, segment đầu bắt đầu bằng P/B
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher/dash
- **Vị trí:** internal/publisher/dash/packager.go:384-389, 711-714, 829-847; internal/publisher/dash/frame_queue.go:242-258

## Hiện tượng
Khi khởi động, segment video đầu không bắt đầu bằng keyframe. Safari/VideoToolbox báo decode error (`-12909`), player khác có thể vỡ hình ở đầu hoặc mất đoạn đầu. Với ABR, shard có audio lệch ranh giới segment so với các shard khác.

## Điều kiện kích hoạt
Frame audio đầu có PTS lớn hơn PTS của IDR đầu dù chỉ 1 ms. Điều này thường xảy ra với luồng có B-frame (PTS của IDR lệch so với audio không theo một chiều cố định) hoặc khi video tới trước.

## Phân tích
```go
threshold := vFirst.PTSms
if aFirst.PTSms > threshold { threshold = aFirst.PTSms }
p.queue.TruncateBefore(threshold)
```
`TruncateBefore` bỏ mọi frame video có PTS nhỏ hơn ngưỡng, kể cả IDR ở đầu queue. `firstIDRSeen` đã true từ `handleH264` và không được đặt lại, nên các P/B tiếp theo trở thành frame đầu của segment.

`findIDRCutPoint` quét từ `i := 1` và không yêu cầu frame 0 là IDR. Với non-audio shard không có truncate (`haveAudio=false`), segment vẫn bắt đầu ở IDR, nên shard audio lệch: ví dụ GOP = segDur thì shard audio cắt ở IDR thứ hai, shard khác cắt ở IDR thứ nhất.

## Vì sao đây là lỗi
Comment của `firstIDRSeen` nói segment 0 phải bắt đầu ở SAP và MPD khai `startWithSAP="1"`. Cổng này chạy trước truncate, nên truncate phá vỡ đảm bảo đó. `TestTruncateBefore` chỉ kiểm tra chia theo PTS.

## Hướng sửa
Sau truncate, nếu frame đầu không phải IDR thì bỏ tiếp tới IDR kế và cắt audio theo PTS của IDR đó. Hoặc không truncate video, chỉ bỏ audio trước IDR rồi căn tfdt theo độ lệch.

## Cách xác minh
Unit test: queue V bắt đầu IDR PTS=1000, A PTS=1010; sau `truncateAtPairingLocked`, frame video đầu phải là IDR. Thực tế: kiểm tra `seg_v_00001.m4s` có sample đầu là sync sample.
