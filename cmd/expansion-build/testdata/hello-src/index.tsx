import { Card } from "@stagehand/console-ui";

function HelloTab(props: { expansionId: string; certname: string }) {
  return <Card>{"Hello from " + props.expansionId + " on " + props.certname}</Card>;
}

export default { contract_version: 1, slots: { nodeDetailTab: HelloTab } };
