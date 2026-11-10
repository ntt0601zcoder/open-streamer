# DASH đóng băng live edge khi audio mất giữa chừng do còn sót frame trong queue
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher/dash
- **Vị trí:** internal/publisher/dash/packager.go:636-649, 742-745; internal/publisher/dash/segmenter.go:292-302; internal/publisher/dash/packager_test.go:908

## Hiện tượng
Nguồn ngừng audio nhưng vẫn có video: MPD ngừng cập nhật vô thời hạn, không có segment mới, và log có thể báo tràn queue video.

## Điều kiện kích hoạt
Stream V+A đang Live, audio ngừng nhưng không có SessionStart (encoder hoặc transcoder chỉ mất audio). Tại thời điểm đó queue audio còn một phần segment, tức là trường hợp thông thường.

## Phân tích
Track chỉ bị coi là chết khi queue rỗng:
```go
if haveAudio && p.queue.AudioLen() == 0 && now.Sub(p.lastAudioFrameAt) >= trackLossTimeout {
    haveAudio = false
}
```
Sau cut cuối, audio còn lại R frame với `0 < R < targetA`. `buildCutDecision` giữ cut:
```go
if q.AudioLen() < targetA { return CutDecision{} }
```
`haveAudio` vẫn true vì queue không rỗng, nên cut bị giữ mãi. Nhánh safety-net cũng đi qua `buildCutDecision` nên bị chặn giống vậy. Không còn frame audio mới để queue đầy thêm.

## Vì sao đây là lỗi
Đây chính là hiện tượng mà `liveTrackPresence` (A-8) muốn tránh. Test chỉ kiểm tra `liveTrackPresence` với queue rỗng, không kiểm tra queue còn dư. Queue chỉ thoát được nếu R đúng bằng bội của target, rất hiếm.

## Hướng sửa
Coi track là chết theo thời gian không nhận frame, không phụ thuộc queue rỗng. Khi audio chết thì xả phần dư cùng cut kế tiếp.

## Cách xác minh
Unit test: Live, queue audio còn 10 frame, `lastAudioFrameAt` cũ hơn `trackLossTimeout`, video có IDR đủ điều kiện; `tryCut` phải cắt video.
