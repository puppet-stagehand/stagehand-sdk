// index.tsx
import { Card } from "/runtime/v1/console-ui.js";
import { jsx } from "/runtime/v1/jsx-runtime.js";
function HelloTab(props) {
  return /* @__PURE__ */ jsx(Card, { children: "Hello from " + props.expansionId + " on " + props.certname });
}
var index_default = { contract_version: 1, slots: { nodeDetailTab: HelloTab } };
export {
  index_default as default
};
