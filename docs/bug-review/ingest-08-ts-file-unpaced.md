# file:// .ts đọc với tốc độ đĩa, không có pacing theo thời gian thực
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/pull, ingestor, timeline, buffer
- **Vị trí:** internal/ingestor/pull/file.go:7-8, 104-110, 161-186; internal/ingestor/reader.go:116-124; internal/ingestor/worker.go:382-407; internal/domain/defaults.go:119

## Hiện tượng
Input `file://<mount>/x.ts` chiếm trọn một nhân CPU, hub báo drop rất cao, playlist HLS/DASH xoay rất nhanh và người xem bị nhảy hình hoặc giật. File không lặp kết thúc trong vài giây rồi báo EOF và failover.

## Điều kiện kích hoạt
Input file có phần mở rộng `.ts`, `.mts`, `.m2ts` hoặc đuôi lạ (đi nhánh default). `loop` mặc định true khiến tình trạng lặp vô hạn.

## Phân tích
`tsHandler.read` chỉ đọc và trả ngay:
```go
n, err := h.f.Read(h.buf)
if n > 0 {
    out := make([]byte, n)
    copy(out, h.buf[:n])
    return out, nil
}
```
MP4 và FLV có `pace()` theo DTS, còn nhánh TS thì không. `NewPacketReader` bọc thẳng bằng `TSPassthroughPacketReader`, không có `WithRealtimePacing`. `readLoop` không sleep.

Ở sau đó không có lớp nào hãm tốc độ. Hub không chặn ghi nên subscriber (1024 gói) tràn trong vài ms và bị drop. Normaliser có `DefaultPTSMaxAheadMs = 0`, còn drift so với `max(actualNow, lastOutputDts)` nên burst được coi là bình thường và timeline chạy vượt xa wallclock.

## Vì sao đây là lỗi
`ARCHITECTURE.md` mô tả `file://` là "paced playback". MP4/FLV trong cùng file được pace để mô phỏng nguồn live. Header của `file.go` ghi "no pacing" là mô tả code, nhưng trong đường ống live không có consumer nào chấp nhận được burst này. Invariant hub không chặn ghi khiến nguồn nhanh mất dữ liệu.

## Hướng sửa
Pace chunk TS theo PCR/DTS: có thể dùng scanner nhẹ, hoặc tính bitrate từ PCR và ngủ theo byte. Reset mốc pace ở mỗi lần lặp như MP4 đang làm, và điều chỉnh timestamp khi lặp để timeline liên tục.

## Cách xác minh
Test: `FileReader` với file TS vài chục MB, đo thời gian đọc hết so với thời lượng media. Thủ công trên môi trường thử: dùng một input `.ts` 10 phút, theo dõi `buffer_drops_total`, CPU và thời gian đến khi EOF khi `loop=false`.
