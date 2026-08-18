import Foundation

public struct ToolResultContentItem: @unchecked Sendable {
    let dictionary: [String: Any]

    public static func text(_ text: String) -> ToolResultContentItem {
        ToolResultContentItem(
            dictionary: [
                "type": "text",
                "text": text,
            ]
        )
    }

    public static func pngImage(_ data: Data) -> ToolResultContentItem {
        ToolResultContentItem(
            dictionary: [
                "type": "image",
                "data": data.base64EncodedString(),
                "mimeType": "image/png",
            ]
        )
    }
}

public struct ToolCallResult: @unchecked Sendable {
    public let content: [ToolResultContentItem]
    public let isError: Bool
    // Optional structured payload (get_app_state metadata, snapshot-ref error
    // envelopes). Serialized only when non-nil; wire-only decoration such as
    // resultType/_meta is added by the modern adapter, never here.
    public let structuredContent: [String: Any]?

    public init(
        content: [ToolResultContentItem],
        isError: Bool = false,
        structuredContent: [String: Any]? = nil
    ) {
        self.content = content
        self.isError = isError
        self.structuredContent = structuredContent
    }

    public var primaryText: String? {
        content.first(where: { $0.dictionary["type"] as? String == "text" })?.dictionary["text"] as? String
    }

    public var asDictionary: [String: Any] {
        var dictionary: [String: Any] = [
            "content": content.map(\.dictionary),
            "isError": isError,
        ]
        if let structuredContent {
            dictionary["structuredContent"] = structuredContent
        }
        return dictionary
    }

    public static func text(_ text: String, isError: Bool = false) -> ToolCallResult {
        ToolCallResult(content: [.text(text)], isError: isError)
    }
}
