# Audio re-encode gán gần như cùng PTS cho mọi frame trong một PES nhiều ADTS

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder/native, internal/tsdemux, internal/ingestor/tsnorm
- **Vị trí:** internal/transcoder/native/audio_reencode.go:177-195, internal/transcoder/native/audio_reencode.go:201-221, internal/transcoder/native/audio_reencode.go:300, internal/transcoder/native/audio_reencode.go:377-378, internal/transcoder/native/stream_pipeline.go:460-468, internal/transcoder/native/stream_pipeline.go:611-614, internal/transcoder/native/ts_input.go:209-213, internal/tsdemux/demuxer.go:132

## Hiện tượng
Với nguồn TS gộp nhiều frame AAC trong một PES, output re-encode có PTS dạng cụm: `X, X+1, X+2, X+3`, rồi nhảy khoảng 80 ms. Player dùng PTS theo từng frame có thể coi là chồng lấn và bỏ frame, nghe như tiếng giật.

## Điều kiện kích hoạt
Nguồn raw-TS (UDP, SRT, HLS) có PES audio chứa từ hai frame ADTS trở lên, và `audio.copy=false`.

## Phân tích
`tsdemux` gọi `OnFrame` một lần cho mỗi PES, `ts_input` giữ nguyên `f.data`, và `bufferAudio` gọi `Process(f.data, f.pts)` một lần. Decoder AAC trả nhiều frame từ một packet, nhưng `drainDecoder` truyền cùng `srcPTS` cho từng frame. Sau mỗi frame:

```go
a.fifoHeadPTS = srcPTS - int64(a.fifo.Size())*1000/int64(a.outRate)
```

FIFO đã được rút gần hết ở frame trước, nên `Size()` lại xấp xỉ 1024 và `fifoHeadPTS` quay về `srcPTS - 21`. Bốn frame trong PES đều có `outPTS` xấp xỉ nhau, sau đó `rebaseAudioPTS` chỉ tách chúng bằng tie-break `+1 ms`.

## Vì sao đây là lỗi
Comment ở dòng 291-299 giả định "the newest buffered sample corresponds to this frame's source PTS", điều này sai với frame thứ hai trở đi của một PES gộp. `tsnorm` remux theo PES nên không tách bundle. Đường passthrough không bị vì phía publisher có `splitADTSBundle`, nhưng đường re-encode tách mỗi frame thành một packet riêng với PTS đã sai. `audio_resample_test.go` không bao phủ PES gộp.

## Hướng sửa
Tính PTS từng frame đã decode trong packet theo số sample đã giải mã: `srcPTS + decodedSamplesSoFar*1000/srcRate`. Hoặc tách ADTS trước khi gọi `Process`, nội suy PTS theo 1024 sample.

## Cách xác minh
Test đơn vị: encode 4 frame ADTS, nối thành một `data`, gọi `Process` và kiểm tra các `PTS` output cách nhau khoảng 21 ms. Thực tế: ffprobe `-show_packets` trên audio HLS của nguồn TS gộp.
