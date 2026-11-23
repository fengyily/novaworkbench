// SubTaskTree — the left-hand navigation tree of the sub-task split panel.
//
// Pure display. It renders the rows the parent already computed and
// reports intent back through callbacks. It owns no data, no selection
// and no collapse state, never fetches, never polls and never keeps a
// timer: the parent stays the single source of truth, so the tree and
// the detail column can never disagree about what exists, or about what
// is selected.
//
// Everything rendered here is a symbol or a number. The component carries
// no i18n keys and no user-facing sentence — titles and durations arrive
// already formatted by the parent.

import { useCallback, useMemo, useRef, useState } from 'react';
import type { CSSProperties, KeyboardEvent } from 'react';
import './SubTaskTree.css';

/** Status vocabulary — the same five states the sub-task API reports. */
export type SubTaskTreeStatus = 'pending' | 'running' | 'done' | 'error' | 'stopped';

/** The two marks a row can carry. */
export type SubTaskTreeKind = 'review' | 'auto';

export interface SubTaskTreeNode {
  id: string;
  title: string;
  status: SubTaskTreeStatus;
  /** 0 for a root row, 1 for its children, and so on. */
  depth: number;
  /**
   * True when the row has at least one descendant. The parent computes
   * this from the full tree rather than from the visible list: a
   * collapsed parent still has children and has to keep its expander,
   * otherwise that subtree could never be unfolded again.
   */
  hasChildren: boolean;
  /** 1-based position inside `nodes`. */
  index: number;
  /** Number of visible rows. */
  total: number;
  kind?: SubTaskTreeKind;
  /** Already-formatted duration; empty renders the placeholder. */
  durationText?: string;
  /** An empty value counts as "no job" — the same rule the card uses. */
  jobId?: string;
  batchId?: string;
}

export interface SubTaskTreeProps {
  /** The visible pre-order list, already filtered by the parent. */
  nodes: readonly SubTaskTreeNode[];
  selectedId: string;
  onSelect: (id: string) => void;
  /** Ids of the collapsed parents. */
  collapsed: ReadonlySet<string>;
  onToggleCollapse: (id: string) => void;
  /** Ids whose job is running, even if the row still says pending. */
  runningIds: ReadonlySet<string>;
}

/** The two marks, drawn as emoji so neither needs a translated label. */
const KIND_MARK: Record<SubTaskTreeKind, string> = { review: '🔍', auto: '🪄' };

/**
 * One dot class per row. The order is part of the contract: a pending
 * row that no dispatcher owns is a stuck row rather than a queued one, so
 * it wears the warning tone and is not re-coloured by a running flag.
 */
function resolveDotModifier(node: SubTaskTreeNode, isRunning: boolean): string {
  if (node.status === 'pending' && !node.jobId && !node.batchId) return 'is-not-dispatched';
  if (isRunning) return 'is-running';
  if (node.status === 'done') return 'is-done';
  if (node.status === 'error') return 'is-error';
  if (node.status === 'stopped') return 'is-stopped';
  return 'is-pending';
}

/** Two-digit counter: 1 renders as "01". */
function pad2(n: number): string {
  return String(n).padStart(2, '0');
}

/**
 * CSSProperties has no index signature for custom properties, so an
 * inline one needs this explicit intersection before it can be set.
 */
type TreeStyle = CSSProperties & { '--sub-tree-depth': number };

/** Shown where the parent had nothing to give us. */
const PLACEHOLDER = '—';

export function SubTaskTree({
  nodes,
  selectedId,
  onSelect,
  collapsed,
  onToggleCollapse,
  runningIds,
}: SubTaskTreeProps) {
  // The row the user last operated on. Remembering it keeps the tree's
  // single tab stop where the user left it, even when the selection
  // moves elsewhere.
  const [focusedId, setFocusedId] = useState('');
  // Row elements, so the arrow keys can move real DOM focus.
  const rowRefs = useRef<Map<string, HTMLDivElement>>(new Map());

  // Roving tabindex: exactly one row is reachable with Tab. The row the
  // user last operated on wins, then the selected row, then the first.
  const rovingId = useMemo(
    () => focusedId || selectedId || nodes[0]?.id || '',
    [focusedId, selectedId, nodes],
  );

  // Move the focus one visible row up or down. `nodes` is already the
  // filtered, visible order, so the array order is the visual order.
  const focusRow = useCallback(
    (delta: number) => {
      const current = nodes.findIndex((n) => n.id === rovingId);
      const target = nodes[current + delta];
      if (!target) return;
      setFocusedId(target.id);
      rowRefs.current.get(target.id)?.focus();
    },
    [nodes, rovingId],
  );

  const handleSelect = useCallback(
    (node: SubTaskTreeNode) => {
      setFocusedId(node.id);
      onSelect(node.id);
    },
    [onSelect],
  );

  const handleKeyDown = useCallback(
    (e: KeyboardEvent<HTMLDivElement>, node: SubTaskTreeNode) => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault(); // Space would scroll the page.
        onSelect(node.id);
        return;
      }
      if (e.key === 'ArrowLeft') {
        // Collapse, but only when there is a subtree and it is open.
        if (node.hasChildren && !collapsed.has(node.id)) {
          e.preventDefault();
          onToggleCollapse(node.id);
        }
        return;
      }
      if (e.key === 'ArrowRight') {
        // Expand, but only when there is a subtree and it is closed.
        if (node.hasChildren && collapsed.has(node.id)) {
          e.preventDefault();
          onToggleCollapse(node.id);
        }
        return;
      }
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        focusRow(1);
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        focusRow(-1);
      }
    },
    [collapsed, focusRow, onSelect, onToggleCollapse],
  );

  return (
    <div className="sub-tree" role="tree">
      {nodes.map((node) => {
        const isSelected = node.id === selectedId;
        const isRunning = node.status === 'running' || runningIds.has(node.id);
        const modifier = resolveDotModifier(node, isRunning);
        const isCollapsed = collapsed.has(node.id);
        return (
          <div
            key={node.id}
            ref={(el) => {
              // Block-bodied on purpose: React calls the callback with
              // null on unmount, and that call has to drop the entry or
              // the map keeps a detached node to focus.
              if (el) rowRefs.current.set(node.id, el);
              else rowRefs.current.delete(node.id);
            }}
            role="treeitem"
            aria-selected={isSelected}
            aria-expanded={node.hasChildren ? !isCollapsed : undefined}
            aria-level={node.depth + 1}
            tabIndex={node.id === rovingId ? 0 : -1}
            className={`sub-tree-row${isSelected ? ' is-selected' : ''}`}
            style={{ '--sub-tree-depth': node.depth } as TreeStyle}
            title={node.title}
            onClick={() => handleSelect(node)}
            onFocus={() => setFocusedId(node.id)}
            onKeyDown={(e) => handleKeyDown(e, node)}
          >
            <span
              className="sub-tree-caret"
              aria-hidden="true"
              onClick={(e) => {
                // Never let the expander bubble into the row: clicking
                // the caret folds the subtree, it does not select it.
                e.stopPropagation();
                onToggleCollapse(node.id);
              }}
            >
              {node.hasChildren ? (isCollapsed ? '▸' : '▾') : ''}
            </span>
            <span className={`sub-tree-dot ${modifier}`} aria-hidden="true" />
            <span className="sub-tree-kind">{node.kind ? KIND_MARK[node.kind] : ''}</span>
            <span className="sub-tree-title">{node.title}</span>
            <span className="sub-tree-index">
              {pad2(node.index)}/{pad2(node.total)}
            </span>
            <span className="sub-tree-duration">{node.durationText || PLACEHOLDER}</span>
          </div>
        );
      })}
    </div>
  );
}
