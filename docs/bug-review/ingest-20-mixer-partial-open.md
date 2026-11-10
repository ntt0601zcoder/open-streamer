# Mixer mất trạng thái ABR khi Open thất bại một phần nên lần retry trả EOF giả
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/pull, ingestor, manager
- **Vị trí:** internal/ingestor/pull/mixer.go:163-182, 184-201, 225-233, 330-336, 366-377; internal/ingestor/worker.go:143-168

## Hiện tượng
Stream `mixer://` khởi động khi upstream audio chưa có buffer, ví dụ lúc boot, sẽ báo video upstream closed ngay sau khi audio sẵn sàng. Input bị Degraded/Exhausted không đúng và mất thêm khoảng 10 s nữa, dù cả hai upstream đều chạy.

## Điều kiện kích hoạt
Video upstream có buffer TS (ABR hoặc raw-TS), audio upstream chưa có buffer để `Subscribe` tại lần `Open` đầu. Lần `Open` kế tiếp thành công trên cùng reader.

## Phân tích
Khi audio lỗi, video bị dọn và mất đối tượng ABR:
```go
if err := r.openAudioSource(ctx); err != nil {
    r.closeVideoSource()
    r.pumpStop()
    return err
}
```
```go
if r.videoInner != nil {
    _ = r.videoInner.Close()
    r.videoInner = nil
}
```
`openSource` retry trên cùng reader. Lần sau `videoInner == nil` nên `openVideoSource` đi nhánh subscribe trực tiếp trên buffer TS. `r.videoChan` vẫn giữ channel cũ, đã bị `pumpInner` đóng. Trong `ReadPackets`, `videoABRCh()` trả channel đó, select nhận `!ok` ngay:
```go
case avp, ok := <-videoABRCh:
    if !ok {
        return nil, fmt.Errorf("mixer reader: video upstream %q closed: %w", r.video, io.EOF)
    }
```
Worker coi là EOF và thoát. Trong khoảng trước khi pump đóng, các gói TS bị bỏ do `pkt.AV == nil`.

## Vì sao đây là lỗi
Comment hứa "On any failure, partial state is rolled back", nhưng rollback phá trạng thái dựng ở constructor và bỏ sót `videoChan`. `openSource` được thiết kế để retry cùng reader. `mixer_test.go` không phủ retry sau lỗi một phần.

## Hướng sửa
Không nil hoá `videoInner`: dùng reader có thể mở lại, hoặc dựng lại inner + chunk reader, và đặt `videoChan = nil`, chờ `pumpWg` trong rollback. Hoặc giữ cấu hình TS riêng khỏi trạng thái mở.

## Cách xác minh
Unit test: buffer video TS có, audio chưa `Create`; gọi `Open` (lỗi), `Create` audio, `Open` (thành công), rồi `ReadPackets` nhận lỗi EOF.
