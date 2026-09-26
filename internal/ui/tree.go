package ui

import (
	"context"
	"fmt"
	"time"

	"dibi/internal/config"
	"dibi/internal/db"

	qt "github.com/mappu/miqt/qt6"
)

// treeEntry is the data attached to a tree item.
type treeEntry struct {
	conn    string
	node    db.Node
	loaded  bool
	loading bool
}

// objectTree is the left panel: connections and their objects, loaded
// lazily when a node is expanded.
type objectTree struct {
	app     *App
	widget  *qt.QTreeWidget
	entries map[uintptr]*treeEntry
	items   map[uintptr]*qt.QTreeWidgetItem
}

func newObjectTree(app *App) *objectTree {
	t := &objectTree{app: app, widget: qt.NewQTreeWidget2(),
		entries: map[uintptr]*treeEntry{}, items: map[uintptr]*qt.QTreeWidgetItem{}}
	w := t.widget
	w.SetHeaderHidden(true)
	w.SetUniformRowHeights(true)
	w.SetContextMenuPolicy(qt.CustomContextMenu)
	w.OnItemExpanded(t.expand)
	w.OnItemClicked(func(item *qt.QTreeWidgetItem, _ int) { t.activate(item) })
	w.OnCustomContextMenuRequested(t.contextMenu)
	return t
}

func connIcon(typ config.Type) *qt.QIcon {
	if typ.IsFile() {
		return icon("drive-harddisk", qt.QStyle__SP_DriveHDIcon)
	}
	return icon("network-server", qt.QStyle__SP_DriveNetIcon)
}

func nodeIcon(k db.Kind) *qt.QIcon {
	switch k {
	case db.KindDatabase:
		return icon("server-database", qt.QStyle__SP_DriveHDIcon)
	case db.KindSchema, db.KindGroup:
		return icon("folder", qt.QStyle__SP_DirIcon)
	case db.KindTable:
		return icon("x-office-spreadsheet", qt.QStyle__SP_FileDialogDetailedView)
	case db.KindView, db.KindMatView:
		return icon("text-x-generic-template", qt.QStyle__SP_FileDialogContentsView)
	case db.KindFunction, db.KindProcedure, db.KindPackage, db.KindTrigger:
		return icon("text-x-script", qt.QStyle__SP_FileIcon)
	case db.KindIndex:
		return icon("view-sort-ascending", qt.QStyle__SP_ArrowUp)
	case db.KindSequence:
		return icon("format-list-numbered", qt.QStyle__SP_FileIcon)
	}
	return icon("text-x-generic", qt.QStyle__SP_FileIcon)
}

// reload rebuilds the tree from the saved connections.
func (t *objectTree) reload() {
	t.widget.Clear()
	t.entries = map[uintptr]*treeEntry{}
	t.items = map[uintptr]*qt.QTreeWidgetItem{}
	for _, c := range t.app.conns {
		item := qt.NewQTreeWidgetItem4(t.widget, []string{c.Name})
		item.SetIcon(0, connIcon(c.Type))
		item.SetToolTip(0, connDescription(c))
		t.add(item, &treeEntry{conn: c.Name, node: db.Node{Kind: db.KindConnection, Name: c.Name}})
	}
}

func connDescription(c config.Connection) string {
	if c.Type.IsFile() {
		return fmt.Sprintf("%s · %s", c.Type.Label(), c.Path)
	}
	return fmt.Sprintf("%s · %s@%s:%d/%s", c.Type.Label(), c.User, c.Host, c.Port, c.Database)
}

func (t *objectTree) add(item *qt.QTreeWidgetItem, e *treeEntry) {
	k := key(item.UnsafePointer())
	t.entries[k] = e
	t.items[k] = item
	if e.node.HasChildren() {
		item.SetChildIndicatorPolicy(qt.QTreeWidgetItem__ShowIndicator)
	}
}

func (t *objectTree) entry(item *qt.QTreeWidgetItem) *treeEntry {
	if item == nil {
		return nil
	}
	return t.entries[key(item.UnsafePointer())]
}

// removeChildren deletes the children of an item and forgets their entries.
func (t *objectTree) removeChildren(item *qt.QTreeWidgetItem) {
	for _, c := range item.TakeChildren() {
		t.forget(c)
		c.Delete()
	}
}

func (t *objectTree) forget(item *qt.QTreeWidgetItem) {
	for i := 0; i < item.ChildCount(); i++ {
		t.forget(item.Child(i))
	}
	k := key(item.UnsafePointer())
	delete(t.entries, k)
	delete(t.items, k)
}

// activate handles a click: remember the connection and open the info panel.
func (t *objectTree) activate(item *qt.QTreeWidgetItem) {
	e := t.entry(item)
	if e == nil {
		return
	}
	t.app.setLastUsed(e.conn, e.node.Database)
	if e.node.HasInfo() {
		t.app.openInfo(e.conn, e.node)
	}
}

// expand loads the children of an item the first time it is expanded.
func (t *objectTree) expand(item *qt.QTreeWidgetItem) {
	e := t.entry(item)
	if e == nil || e.loaded || e.loading {
		return
	}
	src := t.app.source(e.conn)
	if src == nil {
		return
	}
	t.app.setLastUsed(e.conn, e.node.Database)
	e.loading = true
	loading := qt.NewQTreeWidgetItem7(item, []string{"Loading…"})
	loading.SetForeground(0, qt.NewQBrush3(qt.NewQColor6("#888888")))
	node := e.node
	type result struct {
		nodes []db.Node
		err   error
	}
	async(func() result {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		nodes, err := src.Children(ctx, node)
		return result{nodes, err}
	}, func(r result) {
		if t.entry(item) != e { // the item was removed meanwhile
			return
		}
		e.loading = false
		t.removeChildren(item)
		if r.err != nil {
			errItem := qt.NewQTreeWidgetItem7(item, []string{"Error: " + firstLine(r.err.Error(), 80)})
			errItem.SetForeground(0, qt.NewQBrush3(qt.NewQColor6("#b00020")))
			errItem.SetToolTip(0, r.err.Error())
			t.app.setStatus(fmt.Sprintf("Cannot load %s: %v", node.Name, r.err))
			return
		}
		e.loaded = true
		if len(r.nodes) == 0 {
			empty := qt.NewQTreeWidgetItem7(item, []string{"(empty)"})
			empty.SetForeground(0, qt.NewQBrush3(qt.NewQColor6("#888888")))
			return
		}
		for _, n := range r.nodes {
			child := qt.NewQTreeWidgetItem7(item, []string{n.Name})
			child.SetIcon(0, nodeIcon(n.Kind))
			t.add(child, &treeEntry{conn: e.conn, node: n})
		}
	})
}

// refresh reloads the children of an item.
func (t *objectTree) refresh(item *qt.QTreeWidgetItem) {
	e := t.entry(item)
	if e == nil || e.loading {
		return
	}
	t.removeChildren(item)
	e.loaded = false
	if item.IsExpanded() {
		t.expand(item)
	} else {
		item.SetExpanded(true) // triggers expand
	}
}

// connectionItem returns the top-level item of a connection.
func (t *objectTree) connectionItem(name string) *qt.QTreeWidgetItem {
	for i := 0; i < t.widget.TopLevelItemCount(); i++ {
		item := t.widget.TopLevelItem(i)
		if e := t.entry(item); e != nil && e.conn == name {
			return item
		}
	}
	return nil
}

func (t *objectTree) contextMenu(pos *qt.QPoint) {
	item := t.widget.ItemAt(pos)
	e := t.entry(item)
	if e == nil {
		menu := qt.NewQMenu(t.widget.QWidget)
		menu.QWidget.AddActionWithText("New Connection…").OnTriggered(t.app.newConnection)
		menu.ExecWithPos(t.widget.Viewport().MapToGlobalWithQPoint(pos))
		return
	}
	conn := e.conn
	menu := qt.NewQMenu(t.widget.QWidget)
	add := func(text string, f func()) { menu.QWidget.AddActionWithText(text).OnTriggered(f) }
	add("New SQL Script", func() {
		t.app.setLastUsed(conn, e.node.Database)
		t.app.newScript("", "")
	})
	if e.node.Kind == db.KindTable || e.node.Kind == db.KindView || e.node.Kind == db.KindMatView {
		add("Open Info Panel", func() { t.app.openInfo(conn, e.node) })
	}
	if e.node.HasChildren() {
		add("Refresh", func() { t.refresh(item) })
	}
	if e.node.Kind == db.KindConnection {
		menu.AddSeparator()
		add("Edit Connection…", func() { t.app.editConnection(conn) })
		add("Disconnect", func() { t.app.disconnect(conn) })
		add("Delete Connection…", func() { t.app.deleteConnection(conn) })
	}
	menu.ExecWithPos(t.widget.Viewport().MapToGlobalWithQPoint(pos))
}

// collapseConnection removes the loaded children of a connection, so that
// it reconnects when expanded again.
func (t *objectTree) collapseConnection(name string) {
	item := t.connectionItem(name)
	if item == nil {
		return
	}
	t.removeChildren(item)
	if e := t.entry(item); e != nil {
		e.loaded = false
	}
	item.SetExpanded(false)
}
