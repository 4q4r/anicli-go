package tui

import "testing"

func TestPinListZeroItemsBackless(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("zero-item backless menu must not panic: %v", r)
		}
	}()
	m := NewPinList(NewMenuWithoutBack("Пусто", "Ничего"), 10)
	_ = m.Render()
	m.MoveDown()
	m.MoveUp()
	m.Jump(3)
	lo, hi := m.VisibleBody()
	if lo != 0 || hi != 0 {
		t.Fatalf("empty window expected, got [%d,%d)", lo, hi)
	}
}
