# Graph GPU có watermark ảnh phải chạy đuổi từ 0 tới PTS nguồn, gây đứng pipeline lâu

- **Mức độ:** Cao
- **Độ tin cậy:** Trung bình
- **Module liên quan:** internal/transcoder/native, internal/timeline
- **Vị trí:** internal/transcoder/native/cuda.go:172, internal/transcoder/native/cuda.go:188-192, internal/transcoder/native/cuda.go:220, internal/transcoder/native/watermark.go:254, internal/transcoder/native/stream_pipeline.go:1409-1416, internal/transcoder/native/stream_pipeline.go:1120-1124, internal/timeline/normaliser.go:291-302, internal/timeline/normaliser.go:399

## Hiện tượng
Host NVENC, stream có watermark ảnh, input chạy lâu. Khi subprocess respawn hoặc input reconnect, tất cả rendition đứng rất lâu (phút trở lên tùy uptime), CPU tăng cao, không có lỗi. Mỗi lần dựng lại graph đều lặp lại.

## Điều kiện kích hoạt
1. `hw=nvenc` và đường GPU đang hoạt động, watermark `type=image`.
2. PTS nguồn đã lớn: ingest đã chạy nhiều giờ trong cùng worker.
3. Graph được dựng mới: subprocess respawn hoặc `SwitchInput` do reconnect cùng worker.

## Phân tích
Graph GPU nhận frame decoder với PTS nguồn, vì `SetPts(pts)` chỉ được gọi sau `Scale`:

```go
scaled, err = p.gpuScalers[i].Scale(f)
...
scaled.SetPts(pts)
```

Nguồn watermark bắt đầu từ 0 và tăng theo frame:

```go
movieSrc := "movie='...':loop=0,setpts=N/(FRAME_RATE*TB)"
```

PTS nguồn là thời gian từ `wallOrigin` của Normaliser, và `OnSession` không reset `wallOrigin`, nên reconnect không đưa PTS về gần 0. `overlay` đồng bộ qua framesync, phải tiêu thụ frame `movie` cho tới khi vượt PTS main. Sau 24 giờ ở 25 fps là khoảng 2,16 triệu lần decode ảnh, chạy đồng bộ trong một lệnh gọi `Scale`, và lặp lại với mỗi rendition.

## Vì sao đây là lỗi
Đường CPU không bị vì `Watermarker` nhận frame đã `SetPts(pts)` với PTS đã rebase và graph chỉ dựng một lần. Supervisor không có watchdog cho subprocess bị đứng. Hành vi framesync được suy ra từ kiến thức về libavfilter, chưa chạy thử trong đợt review này.

## Hướng sửa
Đặt PTS đã rebase, hoặc PTS tương đối so với thời điểm dựng graph, lên frame trước khi `AddFrame`. Hoặc dùng ảnh tĩnh một frame với `eof_action=repeat`/`loop` ở mức filter thay vì `movie` lặp vô hạn.

## Cách xác minh
Trong builder image có CUDA: đưa frame PTS `86_400_000` đầu tiên vào graph ảnh mới và đo thời gian `Scale`, so với PTS `1`. Có thể mô phỏng ở CPU bằng cùng filter graph với main bắt đầu ở PTS lớn.
