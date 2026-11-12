# Mất audio giữa chừng làm DVR ngừng cắt fragment vĩnh viễn

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/dvr/blob, internal/publisher/dash
- **Vị trí:** internal/dvr/blob/writer.go:195-205, internal/publisher/dash/segmenter.go (`buildCutDecision`, nhánh `q.AudioLen() < targetA`), internal/publisher/dash/packager.go:619-627 (`liveTrackPresence`), internal/dvr/blob/service.go:183-189

## Hiện tượng
Status vẫn là `recording` nhưng archive không có thêm fragment nào sau thời điểm audio dừng. Live HLS/DASH vẫn hoạt động bình thường. Log không báo lỗi.

## Điều kiện kích hoạt
Lane audio-source đã có `audioInit`, sau đó audio ngừng trong khi video tiếp tục. Ví dụ encoder tắt tiếng hoặc failover sang input chỉ có video.

## Phân tích
Writer chỉ dựa vào init để coi là có audio, và init không bao giờ bị xoá:
```go
haveAudio := w.isAudio && w.audioInit != nil
...
dec := w.seg.Cut(now, w.q, haveVideo, haveAudio, audioSR)
```
`buildCutDecision` giữ cut khi thiếu audio, gồm cả cut safety-net vì cả hai đều đi qua hàm này:
```go
if q.AudioLen() < targetA {
    return CutDecision{}
}
```
`targetA > 0`, audio queue rỗng, nên `dec.Ok` luôn false. Video queue chỉ bị cắt ở mốc 30 giây và frame cũ bị bỏ.

## Vì sao đây là lỗi
Packager live đã sửa đúng lỗi này qua `liveTrackPresence` (ghi chú A-8: track có init nhưng chết giữa session sẽ "hold every cut forever"). Writer DVR dùng lại `Segmenter` nhưng không có lớp bảo vệ tương ứng. Tick mỗi giây của lane cũng chỉ gọi `drainCuts`, không thoát được tình huống này.

## Hướng sửa
Theo dõi thời điểm audio gần nhất trong `profileWriter`, coi audio là vắng mặt khi quá ngưỡng như `liveTrackPresence`; ghi fragment video-only và đánh dấu gap.

## Cách xác minh
Unit test: `Ingest` video + AAC vài giây để có init, sau đó chỉ video trong hơn `maxSegFactor × segDur`, kiểm tra số record trong `.ranges` không tăng.
