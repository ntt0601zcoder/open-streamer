# Frame của session cũ lọt vào queue DASH sau boundary, làm kẹt việc cắt segment
- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** publisher/dash, tsdemux, ingestor/tsnorm, ingestor (worker)
- **Vị trí:** internal/publisher/dash/packager.go:298-313, 605-616; internal/publisher/dash/demux.go:173-181; internal/ingestor/tsnorm/tsnorm.go:508-529; internal/ingestor/worker.go:96; internal/publisher/dash/frame_queue.go:178-192, 349-359; internal/publisher/dash/segmenter.go:170, 212; go-astits@v1.15.0 packet_pool.go:25-52, data.go:217-220, data_pes.go:493-494

## Hiện tượng
Sau failover trên nguồn raw-TS, DASH ngừng ra video segment khoảng 2-3 phút (hoặc tới khi PTS mới vượt PTS cũ), rồi tự chạy lại. Log có `dash: video OOO push`. HLS không bị.

## Điều kiện kích hoạt
1. Nguồn raw-TS không transcode, đã chạy đủ lâu.
2. Frame video cuối của worker cũ có PES dài hơn 65535 byte (thường là IDR hoặc nguồn bitrate cao).
3. Failover sang worker mới, PTS về gần 0.

## Phân tích
Demuxer DASH sống qua các session. Với PES lớn, `PacketLength=0` (`data_pes.go:493-494`), nên astits giữ frame cho tới PUSI kế tiếp (`isPESComplete` trả false khi length bằng 0).

Thứ tự trên run loop:
1. `p.onSessionBoundary()` xoá queue.
2. Byte của session mới mới được ghi vào `tsBuffer`.

Muxer tsnorm mới đặt `DiscontinuityIndicator` nên `hasDiscontinuity` trả false, và PUSI mới flush frame cũ ra sau khi queue đã xoá. Frame này vào đầu queue với PTS T lớn, rồi frame mới có PTS nhỏ:
- `VideoSpanMS` trả 0 vì `last <= first`, nên `findIDRCutPoint` trả -1.
- `lastCut` đã bị `Reset` nên safety-net theo wallclock không chạy, và cold-start cần span lớn hơn 0.
- `trimVideoToSpan` dừng vì `newPTSms < first`.

Queue chỉ thoát nhờ `videoFrameHardCap`.

## Vì sao đây là lỗi
`SessionStart` được xử lý đồng bộ, còn demux chạy bất đồng bộ và giữ state qua session. Không có gì gắn frame với session của nó. Comment trên segmenter tin "the segmenter trusts what arrives".

## Hướng sửa
Khi có `SessionStart`: dựng lại `tsBuffer` và demuxer cho session mới. Hoặc gắn generation vào callback để bỏ frame của session cũ. Segmenter nên bỏ phần đầu queue khi PTS lùi xa.

## Cách xác minh
Test packager: feed TS có PES video hơn 64 KB, rồi packet `SessionStart` với PES PTS nhỏ từ một `tsnorm` mới. Kiểm tra `FirstVideo().PTSms` thuộc session mới.
