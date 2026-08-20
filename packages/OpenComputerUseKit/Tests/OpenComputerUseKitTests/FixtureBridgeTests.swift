import Foundation
import XCTest
@testable import OpenComputerUseKit

final class FixtureBridgeTests: XCTestCase {
    func testStateFileURLUsesExplicitTestRoot() {
        let root = "/tmp/ocu-fixture-isolation"

        XCTAssertEqual(
            FixtureBridge.stateFileURL(environment: [
                FixtureBridge.stateRootEnvironmentKey: "  \(root)  ",
            ]).path,
            "\(root)/open-computer-use-fixture/state.json"
        )
    }

    func testStateFileURLIgnoresBlankTestRoot() {
        XCTAssertEqual(
            FixtureBridge.stateFileURL(environment: [
                FixtureBridge.stateRootEnvironmentKey: "   ",
            ]).path,
            URL(fileURLWithPath: NSTemporaryDirectory(), isDirectory: true)
                .appendingPathComponent("open-computer-use-fixture", isDirectory: true)
                .appendingPathComponent("state.json")
                .path
        )
    }

    func testCommandScopeAcceptsMatchingIsolatedScope() {
        XCTAssertTrue(
            FixtureBridge.acceptsCommand(
                scope: "/tmp/ocu-fixture-run-a",
                environment: [FixtureBridge.stateRootEnvironmentKey: " /tmp/ocu-fixture-run-a "]
            )
        )
    }

    func testCommandScopeRejectsMismatchedAndUnscopedCommands() {
        let environment = [FixtureBridge.stateRootEnvironmentKey: "/tmp/ocu-fixture-run-a"]

        XCTAssertFalse(
            FixtureBridge.acceptsCommand(
                scope: "/tmp/ocu-fixture-run-b",
                environment: environment
            )
        )
        XCTAssertFalse(FixtureBridge.acceptsCommand(scope: nil, environment: environment))
    }

    func testDefaultFixtureAcceptsOnlyUnscopedCommands() {
        XCTAssertTrue(FixtureBridge.acceptsCommand(scope: nil, environment: [:]))
        XCTAssertTrue(
            FixtureBridge.acceptsCommand(
                scope: nil,
                environment: [FixtureBridge.stateRootEnvironmentKey: "  "]
            )
        )
        XCTAssertFalse(
            FixtureBridge.acceptsCommand(
                scope: "/tmp/ocu-fixture-run-a",
                environment: [:]
            )
        )
    }
}
