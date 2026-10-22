// useRowSelection — selection state for the requirement lists.
// Split out of RequirementBatch.tsx so that module only exports components
// (Vite fast refresh rebinds the whole module when a file exports both).

import { useEffect, useState } from 'react';

/**
 * Selection state for one list surface.
 *
 * `scopeIds` is the id list the page considers "what the user can act on" —
 * for both lists that is the FILTER RESULT (all pages), never just the rows
 * currently rendered. A collapsed sub-requirement is still part of that set,
 * and skipping it would leave survivors behind after a "delete everything",
 * which is the one outcome this feature must not produce.
 */
export function useRowSelection(scopeIds: string[]) {
  const [selected, setSelected] = useState<Set<string>>(new Set());

  // Prune ids that fell out of scope (filter changed, page reloaded, someone
  // else deleted the row). Returns the SAME Set when nothing moved so the
  // dependent memos below don't re-run on every load.
  const scopeKey = scopeIds.join('|');
  useEffect(() => {
    const scope = new Set(scopeIds);
    setSelected(prev => {
      const next = new Set<string>();
      prev.forEach(id => { if (scope.has(id)) next.add(id); });
      return next.size === prev.size ? prev : next;
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [scopeKey]);

  const allSelected = scopeIds.length > 0 && scopeIds.every(id => selected.has(id));
  const someSelected = !allSelected && scopeIds.some(id => selected.has(id));

  return {
    selected,
    count: selected.size,
    allSelected,
    someSelected,
    isSelected: (id: string) => selected.has(id),
    toggle: (id: string) =>
      setSelected(prev => {
        const next = new Set(prev);
        if (next.has(id)) next.delete(id);
        else next.add(id);
        return next;
      }),
    // `allSelected` is read from this render's snapshot, which is exactly
    // the state the click was made against.
    toggleAll: () =>
      setSelected(() =>
        allSelected ? new Set<string>() : new Set(scopeIds),
      ),
    clear: () => setSelected(new Set()),
  };
}
