# PAT/PMT do FromAV ép phát khi thêm track bị xóa trước khi gửi đi
- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** tsmux
- **Vị trí:** internal/tsmux/fromav.go:117,138,211,227; /home/ntt/go/pkg/mod/github.com/asticode/go-astits@v1.15.0/muxer.go (WriteTables, retransmitTables)

## Hiện tượng
Khi video đến trước audio, các audio PES đầu nằm trên PID chưa có trong PMT mà downstream đã biết, nên bị demuxer bỏ qua (khoảng dưới 1 giây hoặc đến IDR kế tiếp). Mất tiếng đầu segment hoặc đầu session.

## Điều kiện kích hoạt
Một track xuất hiện sau track kia trong cùng `FromAV`, ví dụ nguồn MP4/RTMP gửi video trước audio.

## Phân tích
```go
_, _ = f.mux.WriteTables()   // ghi vào f.buf
...
f.buf.Reset()                // xóa PSI vừa ghi
_, _ = f.mux.WriteData(...)
emitPerPacket(f.buf.Bytes(), onPacket)
```
`WriteData` chỉ phát table khi đến chu kỳ 40 PES hoặc gặp RAI trên PCR PID. Packet audio đầu không thỏa điều kiện nào.

## Vì sao đây là lỗi
Comment nói rõ mục đích là "Force PSI emit NOW" để downstream biết PID mới trước PES đầu. `tsnorm` có đoạn tương tự nhưng không bị vì `outBuf` chỉ reset ở đầu `Process`.

## Hướng sửa
Chuyển `f.buf.Reset()` lên đầu `Write`, trước khi có thể gọi `WriteTables`.

## Cách xác minh
Gọi `Write` với video keyframe rồi audio, thu các packet TS: kiểm tra có PMT liệt kê PID 0x101 đứng trước packet đầu của PID 0x101.
