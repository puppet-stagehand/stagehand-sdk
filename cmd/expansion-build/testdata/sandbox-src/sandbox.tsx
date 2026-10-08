import { createRoot } from "react-dom/client";
import { jsx } from "react/jsx-runtime";

function mount(element: HTMLElement, props: { certname: string }) {
  const root = createRoot(element);
  root.render(jsx("div", { children: props.certname }));
  return () => root.unmount();
}

export default { contract_version: 1, slots: { nodeDetailTab: mount } };
