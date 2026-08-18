import Foundation

// Era-specific tool catalogs. The catalog is generated deterministically per era
// (no mutation of a global definition after connection): legacy returns the
// pinned 2025-03-26 surface verbatim; modern returns the same 9 tools in the
// same order, with the explicit snapshot_ref contract layered onto the 7 action
// tools and state-chain wording on get_app_state and list_apps.
enum ToolCatalog {
    // The 7 action tools that bind their effect to one captured window and so
    // require a snapshot_ref in the modern era.
    static let actionToolNames: Set<String> = [
        "click",
        "drag",
        "perform_secondary_action",
        "press_key",
        "scroll",
        "set_value",
        "type_text",
    ]

    static let snapshotRefPropertyName = "snapshot_ref"

    // Pinned cross-platform wording. Identical on every platform and every action
    // tool; appended last in properties and last in the required list.
    static let snapshotRefDescription =
        "Snapshot reference returned by the immediately preceding get_app_state or action result for this app."

    static let modernGetAppStateDescription =
        "Start an app use session if needed, then get the state of the app's key window and return a screenshot and accessibility tree. The result includes a snapshot_ref that every action tool for this app requires; each action result returns a successor snapshot_ref to thread into the next call. This must be called once per assistant turn before interacting with the app. This tool is part of plugin `Computer Use`."

    static let modernListAppsDescription =
        "List the apps on this computer. Returns the set of apps that are currently running, as well as any that have been used in the last 14 days, including details on usage frequency. Call get_app_state next to capture a window and obtain the snapshot_ref that action tools require. This tool is part of plugin `Computer Use`."

    static func forEra(_ era: ProtocolEra) -> [ToolDefinition] {
        switch era {
        case .legacy20250326:
            return ToolDefinitions.all
        case .modern20260728:
            return ToolDefinitions.all.map(modernize)
        }
    }

    private static func modernize(_ tool: ToolDefinition) -> ToolDefinition {
        if actionToolNames.contains(tool.name) {
            return ToolDefinition(
                name: tool.name,
                description: tool.description,
                annotations: tool.annotations,
                inputSchema: schemaRequiringSnapshotRef(tool.inputSchema)
            )
        }

        if tool.name == "get_app_state" {
            return ToolDefinition(
                name: tool.name,
                description: modernGetAppStateDescription,
                annotations: tool.annotations,
                inputSchema: tool.inputSchema
            )
        }

        if tool.name == "list_apps" {
            return ToolDefinition(
                name: tool.name,
                description: modernListAppsDescription,
                annotations: tool.annotations,
                inputSchema: tool.inputSchema
            )
        }

        return tool
    }

    // Appends the snapshot_ref string property and marks it required, both last,
    // leaving every existing property and required key untouched.
    private static func schemaRequiringSnapshotRef(_ schema: [String: Any]) -> [String: Any] {
        var out = schema

        var properties = (schema["properties"] as? [String: Any]) ?? [:]
        properties[snapshotRefPropertyName] = [
            "type": "string",
            "description": snapshotRefDescription,
        ]
        out["properties"] = properties

        var required = (schema["required"] as? [String]) ?? []
        required.append(snapshotRefPropertyName)
        out["required"] = required

        return out
    }
}
