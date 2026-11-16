package videoframe

// CountFrameCRCLinesForTest 仅用于外部测试包验证流式 framecrc 行计数与溢出边界。
func CountFrameCRCLinesForTest(initialCount int32, chunks ...[]byte) (int32, error) {
	w := frameCRCCountWriter{count: initialCount}
	var writeErr error
	for _, chunk := range chunks {
		if _, err := w.Write(chunk); err != nil {
			writeErr = err
			break
		}
	}
	return w.finish(writeErr)
}
