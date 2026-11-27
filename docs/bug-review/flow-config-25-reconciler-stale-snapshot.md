# Reconciler có thể khởi động lại stream vừa xoá hoặc vừa disable từ snapshot cũ

- **Mức độ:** Thấp
- **Độ tin cậy:** Trung bình
- **Module liên quan:** internal/coordinator, internal/api/handler, internal/store, internal/manager
- **Vị trí:** internal/coordinator/coordinator.go:941-966, internal/coordinator/coordinator.go:313-319, internal/coordinator/coordinator.go:516, internal/api/handler/stream.go:423-448, internal/api/handler/stream.go:724-725

## Hiện tượng
Hiếm gặp: sau `DELETE /streams/{code}`, pipeline vẫn chạy mà bản ghi đã mất, nên không hiện trong `GET /streams`, reconciler không dừng nó, và chỉ biến mất khi có một DELETE khác hoặc restart process. Tương tự, stream vừa đặt `disabled: true` có thể chạy lại.

## Điều kiện kích hoạt
1. `reconcileOnce` đã `List` xong, và đang ở giữa vòng lặp (dài ra nếu có stream khác đang `Start` đồng bộ).
2. Một API `DELETE`, hoặc `POST` đặt `disabled: true`, chạy đúng lúc đó.
3. Vòng lặp tới stream đó sau khi `mgr.Unregister` đã chạy trong `stopLocked`.

## Phân tích
`reconcileOnce` lấy snapshot một lần, rồi kiểm tra từng stream:

```go
if c.IsRunning(st.Code) {
    continue
}
resolved := c.resolveTemplate(ctx, st)
...
if err := c.Start(ctx, resolved); err != nil {
```

`IsRunning` chuyển false ngay sau `mgr.Unregister`, trước khi `Stop` hoàn tất. `Start` chờ trên lifecycle lock, sau đó `startLocked` thấy không chạy và `stream.Disabled` là false theo snapshot cũ, nên khởi động. Vì `Delete` chỉ xoá store sau `Stop`, reconcile không đọc lại store trước khi `Start`.

## Vì sao đây là lỗi
Comment của `RunReconciler` khẳng định race an toàn, chỉ xét trường hợp "Stop arriving after our IsRunning probe" với stream vẫn nên chạy. Lifecycle lock chỉ tuần tự hoá thao tác, không kiểm tra lại ý định hiện tại.

## Hướng sửa
Trong reconciler, đọc lại `FindByCode` ngay trước `Start`, và bỏ qua nếu không còn hoặc đã `Disabled`. Tốt nhất là kiểm tra trong khi giữ lifecycle lock.

## Cách xác minh
Test coordinator với repo giả chứa hook giữa `List` và `IsRunning`: xoá stream ở giữa rồi kiểm tra không có `Start`.
