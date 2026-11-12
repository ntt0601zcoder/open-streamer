# vSnap/aSnap không reset khi SwitchInput, gây lệch A/V cố định sau failover

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder/native
- **Vị trí:** internal/transcoder/native/stream_pipeline.go:230-231, internal/transcoder/native/stream_pipeline.go:544-550, internal/transcoder/native/stream_pipeline.go:570, internal/transcoder/native/stream_pipeline.go:600-605, internal/transcoder/native/stream_pipeline.go:611, internal/transcoder/native/stream_pipeline.go:1026-1031, internal/transcoder/native/stream_pipeline.go:1104-1109

## Hiện tượng
Stream có lệch khởi động giữa hai track hơn 1 giây (đã được snap). Sau một lần chuyển input, hình và tiếng lệch đúng bằng lượng snap cũ, kéo dài tới khi subprocess khởi động lại.

## Điều kiện kích hoạt
1. Lúc khởi động, track thứ hai lệch hơn `crossTrackSnapMs` so với track đầu, như kịch bản trong `TestRebase_RTMPStartupAudioBeforeVideo_SnapsVideo` (`vSnap` khoảng +20 000 ms).
2. Xảy ra `SwitchInput` sang nguồn có hai track đồng bộ.

## Phân tích
Snap chỉ tính một lần cho mỗi vòng đời pipeline:

```go
if !p.vSeeded {
    p.vSnap = crossTrackSnap(srcPTS+p.ptsOffset, p.lastAudioOut, p.aSeeded)
    p.vSeeded = true
}
...
out := srcPTS + p.ptsOffset + p.vSnap
```

Cả hai đường switch chỉ đặt lại `pendingRebase`, `hasVideoSrc`, `videoSrcIntervalMs`, `hasAudioSrc`, không đụng tới `vSeeded`, `aSeeded`, `vSnap`, `aSnap`. Sau switch, `ptsOffset` được tính lại cho nguồn mới, nhưng snap của nguồn cũ vẫn cộng vào một track. Với `vSnap = +20 000`: video ra ở `base+1+20000`, audio ở `base+1`, lệch 20 giây. Với `vSnap` âm: video rơi dưới `lastVideoOut`, đi theo nhánh `+1 ms` một thời gian rồi nằm sau audio đúng bằng độ lớn snap.

## Vì sao đây là lỗi
Comment mô tả đây là hiệu chỉnh "one-time" cho lệch gốc giữa hai đồng hồ của một nguồn, còn nguồn mới có đồng hồ riêng. Guard `reanchorOnRegress` chỉ chạy khi nguồn nhảy lùi hơn 30 giây nên không xử lý trường hợp này. Không có test nào kết hợp snap và switch.

## Hướng sửa
Trong phần reset chung của `SwitchInput`/`switchInputGPU`, đặt `vSeeded, aSeeded = false, false` và `vSnap, aSnap = 0, 0`. Lưu ý: khi seed lại, track đầu tiên sau switch phải được coi là "first track" chứ không so với `lastXOut` của nguồn cũ. Cần kiểm tra cả điều kiện `otherSeeded`.

## Cách xác minh
Test đơn vị theo mẫu trong repo: tạo snap, giả lập các trường switch, rồi đưa video và audio cùng src và khẳng định chênh lệch output nhỏ hơn 50 ms.
