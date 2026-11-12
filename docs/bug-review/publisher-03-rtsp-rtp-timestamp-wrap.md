# RTP timestamp RTSP hỏng khi tràn uint32 trên stream chạy dài (video ~13,25 giờ, audio ~24,85 giờ)
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (RTSP)
- **Vị trí:** internal/publisher/serve_rtsp.go:881-886, 946-948, 970-975

## Hiện tượng
- Sau khoảng 13,25 giờ pipeline RTSP chạy liên tục, video bị đóng băng hoặc tua nhanh tối đa khoảng 144 giây (25 fps).
- Sau khoảng 24,85 giờ với AAC 48 kHz, audio RTSP hỏng vĩnh viễn cho tới khi stream restart.

## Điều kiện kích hoạt
Một stream bật RTSP chạy liên tục qua mốc tràn: `2^32 / 90000` giây cho video, `2^32 / sampleRate` giây cho audio. `onSessionBoundary` không reset `baseDTS`/`baseAudio`, nên failover không làm mốc này lùi lại.

## Phân tích
RTP timestamp được phép quay vòng modulo 2^32, nhưng phép so sánh lại không hiểu quay vòng:
```go
rtpTS := uint32(rel * 90)
if rtpTS <= sess.lastVideoRTP { rtpTS = sess.lastVideoRTP + 1 }
```
Video: khi `rtpTS` quay về giá trị nhỏ, mỗi frame chỉ tăng 1 tick cho tới khi `lastVideoRTP` tràn về 0 (tối đa khoảng 3600 frame với bước 3600 tick).

Audio tệ hơn vì `lastAudioRTP` chỉ cập nhật khi lớn hơn:
```go
if pkt.Timestamp > sess.lastAudioRTP { sess.lastAudioRTP = pkt.Timestamp }
```
Khi `lastAudioRTP` đạt `0xFFFFFFFF`, `computeAudioRTP` trả `last+1 = 0`, điều kiện `>` sai nên giá trị không đổi nữa. Mọi frame sau đó đều có timestamp 0.

## Vì sao đây là lỗi
Nhánh rebase chỉ chạy khi nguồn nhảy lùi, còn ở đây nguồn vẫn tăng đều. Việc 33-bit unwrap ở `tsdemux` không liên quan vì lỗi nằm ở phép ép về 32 bit trong RTSP.

## Hướng sửa
So sánh theo số học modulo: `int32(rtpTS - last) <= 0` thì mới kẹp, và luôn lưu timestamp cuối bằng phép gán kèm so sánh có dấu.

## Cách xác minh
Unit test: đặt `baseDTS`, `lastVideoRTP`/`lastAudioRTP` sát `2^32`, chạy qua mốc tràn, kiểm tra bước nhảy đúng một frame và audio không kẹt ở 0. Soak test RTSP trên 25 giờ.
