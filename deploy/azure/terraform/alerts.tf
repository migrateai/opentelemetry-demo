# Azure alerts → Slack. Mirrors the SolarWinds alert set (deploy/solarwinds).
#
#   alert rule → action group → Logic App (formats the message) → Slack webhook
#
# Alert names avoid : / # % & * < > ? \\ — Azure rejects them.
#
# Azure posts its own JSON ("common alert schema"), which a Slack webhook does not
# accept, so the Logic App turns it into a Slack message.

# ---------------------------------------------------------------------------
# Delivery: Logic App + action group
# ---------------------------------------------------------------------------

resource "azurerm_logic_app_workflow" "slack" {
  name                = "otel-demo-alerts-to-slack"
  resource_group_name = azurerm_resource_group.demo.name
  location            = azurerm_resource_group.demo.location
  tags                = var.tags
}

resource "azurerm_logic_app_trigger_http_request" "alert" {
  name         = "azure-alert"
  logic_app_id = azurerm_logic_app_workflow.slack.id
  schema       = jsonencode({ type = "object" })
}

data "azurerm_client_config" "current" {}

locals {
  # Logic App expressions over the common alert schema.
  essentials = "triggerBody()?['data']?['essentials']"
  condition  = "triggerBody()?['data']?['alertContext']?['condition']?['allOf']?[0]"
  props      = "triggerBody()?['data']?['customProperties']"
  fired      = "equals(${local.essentials}?['monitorCondition'], 'Fired')"

  # Portal pages the "Open …" buttons point to (set per alert as custom properties).
  portal             = "https://portal.azure.com/#@${data.azurerm_client_config.current.tenant_id}/resource"
  appi_failures      = "${local.portal}${azurerm_application_insights.appi.id}/failures"
  appi_performance   = "${local.portal}${azurerm_application_insights.appi.id}/performance"
  container_insights = "${local.portal}${azurerm_kubernetes_cluster.aks.id}/infrainsights"
  aks_metrics        = "${local.portal}${azurerm_kubernetes_cluster.aks.id}/metrics"

  operator_symbol = "replace(replace(replace(replace(coalesce(${local.condition}?['operator'], ''), 'GreaterThanOrEqual', '≥'), 'LessThanOrEqual', '≤'), 'GreaterThan', '>'), 'LessThan', '<')"

  slack_message = {
    text = "@{if(${local.fired}, 'FIRING', 'RESOLVED')}: @{${local.essentials}?['alertRule']}"
    blocks = [
      {
        type = "header"
        text = {
          type  = "plain_text"
          emoji = true
          text  = "@{if(${local.fired}, ':red_circle: FIRING', ':large_green_circle: RESOLVED')} · @{replace(${local.essentials}?['alertRule'], 'otel-demo - ', '')}"
        }
      },
      {
        type = "section"
        fields = [
          { type = "mrkdwn", text = "*Affected*\n@{body('join-dimensions')}" },
          { type = "mrkdwn", text = "*Value*\n@{formatNumber(float(coalesce(${local.condition}?['metricValue'], 0)), '#,##0.##')} @{coalesce(${local.props}?['unit'], '')}  _(threshold @{${local.operator_symbol}} @{${local.condition}?['threshold']} @{coalesce(${local.props}?['unit'], '')})_" },
          { type = "mrkdwn", text = "*Severity*\n@{replace(replace(replace(replace(${local.essentials}?['severity'], 'Sev0', 'Critical'), 'Sev1', 'Error'), 'Sev2', 'Warning'), 'Sev3', 'Info')}" },
          { type = "mrkdwn", text = "*@{if(${local.fired}, 'Fired', 'Resolved')}*\n@{convertFromUtc(if(${local.fired}, ${local.essentials}?['firedDateTime'], coalesce(${local.essentials}?['resolvedDateTime'], utcNow())), 'India Standard Time', 'dd MMM, HH:mm')} IST" },
        ]
      },
      {
        type     = "context"
        elements = [{ type = "mrkdwn", text = "Azure Monitor · ${var.aks_name} · @{${local.essentials}?['description']}" }]
      },
      {
        type = "actions"
        elements = [
          { type = "button", action_id = "view-alert", text = { type = "plain_text", text = "View alert" }, url = "https://portal.azure.com/#view/Microsoft_Azure_Monitoring_Alerts/AlertDetails.ReactView/alertId/@{encodeUriComponent(${local.essentials}?['alertId'])}" },
          { type = "button", action_id = "open-data", text = { type = "plain_text", text = "@{if(empty(${local.condition}?['linkToFilteredSearchResultsUI']), 'Open metrics', 'Open query results')}" }, url = "@{coalesce(${local.condition}?['linkToFilteredSearchResultsUI'], ${local.props}?['metrics_url'], ${local.props}?['dashboard_url'])}" },
          { type = "button", action_id = "open-dashboard", text = { type = "plain_text", text = "Open dashboard" }, url = "@{${local.props}?['dashboard_url']}" },
        ]
      },
    ]
  }
}

# "Affected" lines: the alert's dimensions as "*Name:* value", one per line.
resource "azurerm_logic_app_action_custom" "select_dimensions" {
  name         = "select-dimensions"
  logic_app_id = azurerm_logic_app_workflow.slack.id
  body = jsonencode({
    type     = "Select"
    runAfter = {}
    inputs = {
      from   = "@coalesce(${local.condition}?['dimensions'], json('[]'))"
      select = "@concat('*', toUpper(substring(item()?['name'], 0, 1)), substring(item()?['name'], 1), ':* ', item()?['value'])"
    }
  })
}

resource "azurerm_logic_app_action_custom" "join_dimensions" {
  name         = "join-dimensions"
  logic_app_id = azurerm_logic_app_workflow.slack.id
  body = jsonencode({
    type     = "Join"
    runAfter = { "select-dimensions" = ["Succeeded"] }
    inputs   = { from = "@body('select-dimensions')", joinWith = "\n" }
  })
}

resource "azurerm_logic_app_action_http" "post_to_slack" {
  name         = "post-to-slack"
  logic_app_id = azurerm_logic_app_workflow.slack.id
  method       = "POST"
  uri          = var.slack_webhook_url
  headers      = { "Content-Type" = "application/json" }
  body         = jsonencode(local.slack_message)

  run_after {
    action_name   = azurerm_logic_app_action_custom.join_dimensions.name
    action_result = "Succeeded"
  }
}

resource "azurerm_monitor_action_group" "slack" {
  name                = "otel-demo-slack"
  resource_group_name = azurerm_resource_group.demo.name
  short_name          = "otel-slack"
  tags                = var.tags

  webhook_receiver {
    name                    = "logic-app-to-slack"
    service_uri             = azurerm_logic_app_trigger_http_request.alert.callback_url
    use_common_alert_schema = true
  }
}

# ---------------------------------------------------------------------------
# SolarWinds → Slack. SolarWinds' own Slack integration has a fixed message
# format, so SolarWinds posts to this Logic App (a "webhook" notification,
# deploy/solarwinds) and the Logic App formats the Slack message.
# ---------------------------------------------------------------------------

resource "azurerm_logic_app_workflow" "solarwinds_slack" {
  name                = "otel-demo-solarwinds-to-slack"
  resource_group_name = azurerm_resource_group.demo.name
  location            = azurerm_resource_group.demo.location
  tags                = var.tags
}

resource "azurerm_logic_app_trigger_http_request" "solarwinds_alert" {
  name         = "solarwinds-alert"
  logic_app_id = azurerm_logic_app_workflow.solarwinds_slack.id
  schema       = jsonencode({ type = "object" })
}

locals {
  # SolarWinds sends the alert JSON base64-encoded ("application/octet-stream").
  swo     = "outputs('parse-payload')"
  cleared = "equals(${local.swo}?['severity'], 'CLEAR')"

  swo_slack_message = {
    text = "@{if(${local.cleared}, 'RESOLVED', 'FIRING')}: @{${local.swo}?['name']}"
    blocks = [
      {
        type = "header"
        text = {
          type  = "plain_text"
          emoji = true
          text  = "@{if(${local.cleared}, ':large_green_circle: RESOLVED', if(equals(${local.swo}?['severity'], 'CRITICAL'), ':red_circle: FIRING', ':large_orange_circle: FIRING'))} · @{replace(${local.swo}?['name'], 'otel-demo: ', '')}"
        }
      },
      {
        type = "section"
        fields = [
          { type = "mrkdwn", text = "*Affected*\n@{body('join-tags')}" },
          { type = "mrkdwn", text = "*Value*\n@{formatNumber(float(coalesce(${local.swo}?['metrics']?[0]?['value'], 0)), '#,##0.###')}  _(@{${local.swo}?['condition']})_" },
          { type = "mrkdwn", text = "*Severity*\n@{if(${local.cleared}, 'Resolved', concat(substring(${local.swo}?['severity'], 0, 1), toLower(substring(${local.swo}?['severity'], 1))))}" },
          { type = "mrkdwn", text = "*@{if(${local.cleared}, 'Resolved', 'Fired')}*\n@{convertFromUtc(coalesce(${local.swo}?['clearedAt'], ${local.swo}?['timestamp'], utcNow()), 'India Standard Time', 'dd MMM, HH:mm')} IST@{if(${local.cleared}, concat(' · after ', string(div(coalesce(${local.swo}?['activeDurationInSeconds'], 0), 60)), ' min'), '')}" },
        ]
      },
      {
        type     = "context"
        elements = [{ type = "mrkdwn", text = "SolarWinds Observability · ${var.aks_name} · @{${local.swo}?['description']}" }]
      },
      {
        type = "actions"
        elements = [
          { type = "button", action_id = "view-alert", text = { type = "plain_text", text = "View alert" }, url = "@{${local.swo}?['detailUrl']}" },
          { type = "button", action_id = "open-metrics", text = { type = "plain_text", text = "Open metrics" }, url = "@{coalesce(${local.swo}?['runbook'], ${local.swo}?['detailUrl'])}" },
        ]
      },
    ]
  }
}

resource "azurerm_logic_app_action_custom" "swo_parse" {
  name         = "parse-payload"
  logic_app_id = azurerm_logic_app_workflow.solarwinds_slack.id
  body = jsonencode({
    type     = "Compose"
    runAfter = {}
    inputs   = "@json(base64ToString(triggerBody()?['$content']))"
  })
}

# "Affected" lines from the alert's tags, with readable names.
resource "azurerm_logic_app_action_custom" "swo_select_tags" {
  name         = "select-tags"
  logic_app_id = azurerm_logic_app_workflow.solarwinds_slack.id
  body = jsonencode({
    type     = "Select"
    runAfter = { "parse-payload" = ["Succeeded"] }
    inputs = {
      from   = "@coalesce(${local.swo}?['affectedEvaluations']?[0]?['tags'], ${local.swo}?['tags'], json('[]'))"
      select = "@concat('*', replace(replace(replace(replace(replace(replace(item()?['key'], 'k8s.pod.name', 'Pod'), 'k8s.node.name', 'Node'), 'service.name', 'Service'), 'sw.transaction', 'Endpoint'), 'http.route', 'Route'), 'mountpoint', 'Mount'), ':* ', item()?['value'])"
    }
  })
}

resource "azurerm_logic_app_action_custom" "swo_join_tags" {
  name         = "join-tags"
  logic_app_id = azurerm_logic_app_workflow.solarwinds_slack.id
  body = jsonencode({
    type     = "Join"
    runAfter = { "select-tags" = ["Succeeded"] }
    inputs   = { from = "@body('select-tags')", joinWith = "\n" }
  })
}

resource "azurerm_logic_app_action_http" "swo_post_to_slack" {
  name         = "post-to-slack"
  logic_app_id = azurerm_logic_app_workflow.solarwinds_slack.id
  method       = "POST"
  uri          = var.solarwinds_slack_webhook_url
  headers      = { "Content-Type" = "application/json" }
  body         = jsonencode(local.swo_slack_message)

  run_after {
    action_name   = azurerm_logic_app_action_custom.swo_join_tags.name
    action_result = "Succeeded"
  }
}
