import React, { useEffect, useState } from "react"
import styled from "styled-components"
import { Alert, combinedAlerts } from "./alerts"
import { LogUpdateAction, LogUpdateEvent, useLogStore } from "./LogStore"
import OverviewResourceBar from "./OverviewResourceBar"
import OverviewResourceDetails from "./OverviewResourceDetails"
import OverviewResourceSidebar from "./OverviewResourceSidebar"
import OverviewTabBar from "./OverviewTabBar"
import { Color } from "./style-helpers"
import { useTabNav } from "./TabNav"
import { ResourceName } from "./types"

type OverviewResourcePaneProps = {
  view: Proto.webviewView
}

let OverviewResourcePaneRoot = styled.div`
  display: flex;
  flex-direction: column;
  width: 100%;
  height: 100vh;
  background-color: ${Color.grayDark};
  max-height: 100%;
`

let Main = styled.div`
  display: flex;
  width: 100%;
  // In Safari, flex-basis "auto" squishes OverviewTabBar + OverviewResourceBar
  flex: 1 1 100%;
  overflow: hidden;
`

export default function OverviewResourcePane(props: OverviewResourcePaneProps) {
  let nav = useTabNav()
  const logStore = useLogStore()
  let resources = props.view?.resources || []
  let name = nav.invalidTab || nav.selectedTab || ""
  let r: Proto.webviewResource | undefined
  let all = name === "" || name === ResourceName.all
  if (!all) {
    r = resources.find((r) => r.name === name)
  }
  let selectedTab = ""
  if (all) {
    selectedTab = ResourceName.all
  } else if (r?.name) {
    selectedTab = r.name
  }

  const [truncateCount, setTruncateCount] = useState<number>(0)

  // add a listener to rebuild alerts whenever a truncation event occurs
  // truncateCount is a dummy state variable to trigger a re-render to
  // simplify logic vs reconciliation between logStore + props
  useEffect(() => {
    const rebuildAlertsOnLogClear = (e: LogUpdateEvent) => {
      if (e.action === LogUpdateAction.truncate) {
        setTruncateCount(truncateCount + 1)
      }
    }

    logStore.addUpdateListener(rebuildAlertsOnLogClear)
    return () => logStore.removeUpdateListener(rebuildAlertsOnLogClear)
  }, [truncateCount])

  let alerts: Alert[] = []
  if (r) {
    alerts = combinedAlerts(r, logStore)
  } else if (all) {
    resources.forEach((r) => alerts.push(...combinedAlerts(r, logStore)))
  }

  // Hide the HTML element scrollbars, since this pane does all scrolling internally.
  // TODO(nick): Remove this when the old UI is deleted.
  useEffect(() => {
    document.documentElement.style.overflow = "hidden"
  })

  return (
    <OverviewResourcePaneRoot>
      <OverviewTabBar selectedTab={selectedTab} />
      <OverviewResourceBar {...props} />
      <Main>
        <OverviewResourceSidebar {...props} name={name} />
        <OverviewResourceDetails resource={r} name={name} alerts={alerts} />
      </Main>
    </OverviewResourcePaneRoot>
  )
}
