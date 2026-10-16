// removeMermaidLeftovers — on a syntax error mermaid v11's render() rejects but
// may also leave its "Syntax error in text" bomb SVG attached to document.body
// (container id `d<id>`). Drop both candidates; missing nodes are a no-op.
export function removeMermaidLeftovers(id: string): void {
  document.getElementById(`d${id}`)?.remove();
  document.getElementById(id)?.remove();
}
