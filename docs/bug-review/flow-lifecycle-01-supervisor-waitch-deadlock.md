# Supervisor treo vĩnh viễn khi subprocess transcoder chết

- **Mức độ:** Nghiêm trọng
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder, internal/coordinator, internal/api, internal/buffer
- **Vị trí:** internal/transcoder/supervisor.go:242-269, internal/transcoder/supervisor.go:106-143, internal/transcoder/service.go:417-418, internal/coordinator/coordinator.go:483-485, internal/coordinator/coordinator.go:515-516

## Hiện tượng
Subprocess `open-streamer-transcoder` crash (SIGSEGV, OOM kill) và không bao giờ được respawn. Rendition đứng hình vĩnh viễn, `restart_count` không tăng, status vẫn Active. Sau đó mọi `DELETE`, `restart`, `PUT` đổi transcoder của stream đó treo mãi, các thao tác lifecycle tiếp theo của stream cũng treo theo.

## Điều kiện kích hoạt
1. Subprocess chết đột ngột, không gửi terminal error.
2. Trong `runOnce`, `select` đầu tiên nhận từ `waitCh` trước `pumpErrCh`. Khi process chết, hai nguồn sẵn sàng gần như cùng lúc nên đây là tình huống thường gặp (Go chọn ngẫu nhiên nếu cả hai đã ready).
3. Sau 3 giây, goroutine kẹt vĩnh viễn ở lần đọc `<-waitCh` thứ hai.

## Phân tích
`waitCh` chỉ nhận đúng một giá trị:
```go
go func() { waitCh <- cmd.Wait() }()
select {
case err := <-pumpErrCh: ...
case err := <-waitCh:    // giá trị duy nhất bị lấy ở đây
    firstErr = fmt.Errorf("subprocess died: %w", err)
case <-subCtx.Done(): ...
}
...
select {
case <-waitCh:            // không bao giờ có thêm giá trị
case <-stopTimer.C:
    _ = cmd.Process.Kill()
    <-waitCh              // block vĩnh viễn
}
```
`runOnce` không return nên `supervisor.Run` không tới `recordError`, `fireUnhealthyIfTransitioned` hay backoff. `sw.done` không đóng. `Service.Stop` chờ `<-sw.done` trong khi `Coordinator.stopLocked` giữ per-stream lifecycle lock, nên Stop không tới `mgr.Unregister` và lock không được nhả. Các defer của `runOnce` cũng không chạy nên subscriber trên `$raw$<code>` không được Unsubscribe.

## Vì sao đây là lỗi
Comment của timer nói mục đích là chặn child treo làm kẹt `Service.Stop`, nhưng nhánh `subprocess died` lại gây đúng tình trạng đó. Reconciler không cứu được vì `IsRunning` vẫn true. Issue #52 crash-loop vẫn respawn được vì đi qua terminal error và `pumpErrCh`, nên lỗi này dễ bị che. Không có test nào phủ `runOnce`.

## Hướng sửa
Ghi nhận trạng thái đã nhận `waitCh` (ví dụ `exited := true`) và bỏ qua vòng chờ thứ hai, hoặc `close(waitCh)` sau khi gửi để đọc lần hai trả về ngay.

## Cách xác minh
Thay binary transcoder bằng script chạy gRPC stub rồi tự `kill -9`, hoặc viết unit test tách phần chờ. Lặp nhiều lần: một phần số lần crash sẽ không có log `subprocess exited, will respawn`, goroutine dump cho thấy `runOnce` đứng ở `<-waitCh`, và `DELETE` stream treo.
