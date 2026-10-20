use std::collections::BTreeMap;
use std::env;
use std::ffi::OsStr;
use std::fs::{self, File, OpenOptions};
use std::io::{Read, Write};
use std::net::Ipv4Addr;
use std::os::unix::io::AsRawFd;
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::thread;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

type Result<T> = std::result::Result<T, Box<dyn std::error::Error>>;

const DEFAULT_FC_SLICE: &str = "qemu.slice";
const CPU_PLACEMENT_DEDICATED: &str = "dedicated";
const CPU_PLACEMENT_SHARED: &str = "shared";
const CPU_PLACEMENT_MANUAL: &str = "manual";
const CPU_RESERVATION_TTL_SECS: u64 = 600;
const FIRECRACKER_TAP_IFACE_PATTERN: &str = "fc+";
const FIRECRACKER_GUEST_CIDR: &str = "192.168.0.0/16";
const LOCK_EX: i32 = 2;
const LOCK_UN: i32 = 8;

unsafe extern "C" {
    fn flock(fd: i32, operation: i32) -> i32;
}

#[derive(Debug, Clone)]
struct RunConfig {
    install_dir: PathBuf,
    id: String,
    vcpus: u32,
    mem_mib: u32,
    fc_slice: String,
    host_cpus: Option<String>,
    host_cpus_explicit: bool,
    cpu_placement_mode: Option<String>,
    cpu_placement_total_requested: Option<u32>,
    cpu_placement_allowed_count: Option<usize>,
    template: String,
    rootfs_image: Option<PathBuf>,
    wait_for_ssh: bool,
}

#[derive(Debug, Clone)]
struct InstancePaths {
    safe_id: String,
    instance_dir: PathBuf,
    api_socket: PathBuf,
    log_file: PathBuf,
    rootfs: PathBuf,
    pid_file: PathBuf,
    state_file: PathBuf,
    stdout_log: PathBuf,
    stderr_log: PathBuf,
}

#[derive(Debug, Clone)]
struct NetConfig {
    tap_dev: String,
    tap_ip: Ipv4Addr,
    guest_ip: Ipv4Addr,
    guest_mac: String,
    host_iface: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct CpuRequest {
    id: String,
    vcpus: u32,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct CpuPlacement {
    id: String,
    vcpus: u32,
    host_cpus: Vec<u32>,
    mode: &'static str,
    total_requested: u32,
    allowed_count: usize,
}

impl CpuPlacement {
    fn affinity(&self) -> String {
        format_cpu_list(&self.host_cpus)
    }
}

#[derive(Debug, Clone)]
struct RunningFirecrackerInstance {
    id: String,
    vcpus: u32,
    pid: u32,
    unit: Option<String>,
    host_cpus: Option<String>,
    cpu_placement_mode: Option<String>,
    state_file: PathBuf,
}

impl RunningFirecrackerInstance {
    fn is_manual_cpu_placement(&self) -> bool {
        self.cpu_placement_mode.as_deref() == Some(CPU_PLACEMENT_MANUAL)
            || (self.cpu_placement_mode.is_none() && self.host_cpus.is_some())
    }
}

#[derive(Debug, Clone)]
struct CpuPlacementClaim {
    id: String,
    vcpus: u32,
    host_cpus: Option<String>,
    cpu_placement_mode: Option<String>,
}

impl CpuPlacementClaim {
    fn is_manual_cpu_placement(&self) -> bool {
        self.cpu_placement_mode.as_deref() == Some(CPU_PLACEMENT_MANUAL)
            || (self.cpu_placement_mode.is_none() && self.host_cpus.is_some())
    }
}

struct PlacementLock {
    file: File,
}

impl PlacementLock {
    fn acquire(install_dir: &Path) -> Result<Self> {
        fs::create_dir_all(install_dir)?;
        let path = install_dir.join("firecrackerctl-placement.lock");
        let file = OpenOptions::new()
            .create(true)
            .read(true)
            .write(true)
            .open(&path)?;
        let rc = unsafe { flock(file.as_raw_fd(), LOCK_EX) };
        if rc != 0 {
            return Err(format!("failed to acquire placement lock: {}", path.display()).into());
        }
        Ok(Self { file })
    }
}

impl Drop for PlacementLock {
    fn drop(&mut self) {
        let _ = unsafe { flock(self.file.as_raw_fd(), LOCK_UN) };
    }
}

struct CpuReservationGuard {
    path: Option<PathBuf>,
}

impl CpuReservationGuard {
    fn remove(&mut self) -> Result<()> {
        if let Some(path) = self.path.take() {
            if let Err(err) = fs::remove_file(&path) {
                if err.kind() != std::io::ErrorKind::NotFound {
                    return Err(format!("remove CPU reservation {}: {err}", path.display()).into());
                }
            }
        }
        Ok(())
    }
}

impl Drop for CpuReservationGuard {
    fn drop(&mut self) {
        let _ = self.remove();
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum RootfsCloneMethod {
    Reflink,
    Copy,
}

impl RootfsCloneMethod {
    fn as_str(self) -> &'static str {
        match self {
            RootfsCloneMethod::Reflink => "reflink",
            RootfsCloneMethod::Copy => "copy",
        }
    }
}

#[derive(Debug, Clone)]
struct RootfsCloneResult {
    method: RootfsCloneMethod,
    source_bytes: u64,
    dest_bytes: u64,
}

struct PhaseTimer {
    start: Instant,
    last: Instant,
    entries: Vec<(&'static str, u128)>,
}

impl PhaseTimer {
    fn start() -> Self {
        let now = Instant::now();
        Self {
            start: now,
            last: now,
            entries: Vec::new(),
        }
    }

    fn mark(&mut self, phase: &'static str) {
        let now = Instant::now();
        self.entries
            .push((phase, now.duration_since(self.last).as_millis()));
        self.last = now;
    }

    fn total_ms(&self) -> u128 {
        self.start.elapsed().as_millis()
    }
}

fn main() {
    if let Err(err) = run() {
        eprintln!("error: {err}");
        std::process::exit(1);
    }
}

fn run() -> Result<()> {
    let mut args: Vec<String> = env::args().skip(1).collect();
    if args.is_empty() {
        print_help();
        return Ok(());
    }

    match args.remove(0).as_str() {
        "run" => cmd_run(parse_run_config(args)?)?,
        "clone" => {
            if args.len() < 2 {
                return Err("usage: firecrackerctl clone TEMPLATE ID [run options]".into());
            }
            let template = args.remove(0);
            let id = args.remove(0);
            let mut cfg = parse_run_config(args)?;
            cfg.template = template;
            cfg.id = id;
            cmd_run(cfg)?;
        }
        "stop" => cmd_stop(parse_stop_args(args)?)?,
        "snapshot" => {
            let opts = parse_snapshot_args(args)?;
            cmd_snapshot(opts)?;
        }
        "storage-info" => cmd_storage_info(parse_install_dir_arg(args)?)?,
        "list-templates" => cmd_list_templates(install_dir_from_env())?,
        "-h" | "--help" | "help" => print_help(),
        other => return Err(format!("unknown command: {other}").into()),
    }

    Ok(())
}

fn print_help() {
    println!(
        "firecrackerctl - Rust lifecycle helper for FastVM Firecracker microVMs\n\n\
Commands:\n  \
run --id ID [--template NAME] [--vcpus N] [--mem-mib N] [--host-cpus LIST]\n  \
clone TEMPLATE ID [run options]\n  \
snapshot --id ID --template NAME [--delete-instance]\n  \
stop --id ID [--delete-instance]\n  \
storage-info [--install-dir DIR]\n  \
list-templates\n\n\
Environment defaults:\n  \
INSTALL_DIR=/opt/fastvm-microvm\n  \
VCPUS=16\n  \
MEM_MIB=65536\n  \
FC_SLICE=qemu.slice\n  \
HOST_CPUS= (auto when empty)\n  \
TEMPLATE=ubuntu"
    );
}

fn parse_run_config(args: Vec<String>) -> Result<RunConfig> {
    let host_cpus = non_empty_env("HOST_CPUS");
    let host_cpus_explicit = host_cpus.is_some();
    let mut cfg = RunConfig {
        install_dir: install_dir_from_env(),
        id: env::var("ID").unwrap_or_else(|_| "fcbench".to_string()),
        vcpus: env_u32("VCPUS", 16)?,
        mem_mib: env_u32("MEM_MIB", 65536)?,
        fc_slice: env::var("FC_SLICE").unwrap_or_else(|_| DEFAULT_FC_SLICE.to_string()),
        host_cpus,
        host_cpus_explicit,
        cpu_placement_mode: if host_cpus_explicit {
            Some(CPU_PLACEMENT_MANUAL.to_string())
        } else {
            None
        },
        cpu_placement_total_requested: None,
        cpu_placement_allowed_count: None,
        template: env::var("TEMPLATE").unwrap_or_else(|_| "ubuntu".to_string()),
        rootfs_image: non_empty_env("ROOTFS_IMAGE").map(PathBuf::from),
        wait_for_ssh: env::var("WAIT_FOR_SSH").map(|v| v != "0").unwrap_or(true),
    };

    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--install-dir" => {
                i += 1;
                cfg.install_dir = PathBuf::from(required_arg(&args, i, "--install-dir")?);
            }
            "--id" => {
                i += 1;
                cfg.id = required_arg(&args, i, "--id")?.to_string();
            }
            "--vcpus" => {
                i += 1;
                cfg.vcpus = required_arg(&args, i, "--vcpus")?.parse()?;
            }
            "--mem-mib" => {
                i += 1;
                cfg.mem_mib = required_arg(&args, i, "--mem-mib")?.parse()?;
            }
            "--slice" => {
                i += 1;
                cfg.fc_slice = required_arg(&args, i, "--slice")?.to_string();
            }
            "--host-cpus" => {
                i += 1;
                cfg.host_cpus = Some(required_arg(&args, i, "--host-cpus")?.to_string());
                cfg.host_cpus_explicit = true;
                cfg.cpu_placement_mode = Some(CPU_PLACEMENT_MANUAL.to_string());
            }
            "--template" => {
                i += 1;
                cfg.template = required_arg(&args, i, "--template")?.to_string();
            }
            "--rootfs-image" => {
                i += 1;
                cfg.rootfs_image = Some(PathBuf::from(required_arg(&args, i, "--rootfs-image")?));
            }
            "--no-wait-ssh" => cfg.wait_for_ssh = false,
            other => return Err(format!("unknown run option: {other}").into()),
        }
        i += 1;
    }

    if cfg.vcpus == 0 {
        return Err("vcpus must be greater than 0".into());
    }
    if cfg.mem_mib == 0 {
        return Err("mem-mib must be greater than 0".into());
    }

    Ok(cfg)
}

fn parse_stop_args(args: Vec<String>) -> Result<(PathBuf, String, bool)> {
    let mut install_dir = install_dir_from_env();
    let mut id = env::var("ID").unwrap_or_else(|_| "fcbench".to_string());
    let mut delete_instance = env::var("DELETE_INSTANCE")
        .map(|v| v == "1")
        .unwrap_or(false);

    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--install-dir" => {
                i += 1;
                install_dir = PathBuf::from(required_arg(&args, i, "--install-dir")?);
            }
            "--id" => {
                i += 1;
                id = required_arg(&args, i, "--id")?.to_string();
            }
            "--delete-instance" => delete_instance = true,
            other => return Err(format!("unknown stop option: {other}").into()),
        }
        i += 1;
    }

    Ok((install_dir, id, delete_instance))
}

#[derive(Debug)]
struct SnapshotArgs {
    install_dir: PathBuf,
    id: String,
    template: String,
    delete_instance: bool,
}

fn parse_snapshot_args(args: Vec<String>) -> Result<SnapshotArgs> {
    let mut opts = SnapshotArgs {
        install_dir: install_dir_from_env(),
        id: env::var("ID").unwrap_or_default(),
        template: env::var("TEMPLATE").unwrap_or_default(),
        delete_instance: env::var("DELETE_INSTANCE")
            .map(|v| v == "1")
            .unwrap_or(false),
    };

    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--install-dir" => {
                i += 1;
                opts.install_dir = PathBuf::from(required_arg(&args, i, "--install-dir")?);
            }
            "--id" => {
                i += 1;
                opts.id = required_arg(&args, i, "--id")?.to_string();
            }
            "--template" => {
                i += 1;
                opts.template = required_arg(&args, i, "--template")?.to_string();
            }
            "--delete-instance" => opts.delete_instance = true,
            other => return Err(format!("unknown snapshot option: {other}").into()),
        }
        i += 1;
    }

    if opts.id.is_empty() || opts.template.is_empty() {
        return Err("usage: firecrackerctl snapshot --id ID --template TEMPLATE".into());
    }

    Ok(opts)
}

fn parse_install_dir_arg(args: Vec<String>) -> Result<PathBuf> {
    let mut install_dir = install_dir_from_env();
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--install-dir" => {
                i += 1;
                install_dir = PathBuf::from(required_arg(&args, i, "--install-dir")?);
            }
            other => return Err(format!("unknown storage-info option: {other}").into()),
        }
        i += 1;
    }
    Ok(install_dir)
}

fn required_arg<'a>(args: &'a [String], index: usize, flag: &str) -> Result<&'a str> {
    args.get(index)
        .map(|s| s.as_str())
        .ok_or_else(|| format!("missing value for {flag}").into())
}

fn install_dir_from_env() -> PathBuf {
    PathBuf::from(env::var("INSTALL_DIR").unwrap_or_else(|_| "/opt/fastvm-microvm".to_string()))
}

fn env_u32(key: &str, fallback: u32) -> Result<u32> {
    match env::var(key) {
        Ok(v) if !v.trim().is_empty() => Ok(v.parse()?),
        _ => Ok(fallback),
    }
}

fn non_empty_env(key: &str) -> Option<String> {
    env::var(key).ok().filter(|v| !v.trim().is_empty())
}

fn cmd_run(mut cfg: RunConfig) -> Result<()> {
    ensure_root()?;
    let mut timings = PhaseTimer::start();

    let fc_bin = cfg.install_dir.join("bin/firecracker");
    let kernel = latest_kernel(&cfg.install_dir)?;
    let ssh_key = cfg.install_dir.join("images/id_rsa");
    let source_rootfs = template_path(&cfg)?;

    require_path(&fc_bin)?;
    require_path(&kernel)?;
    require_path(&ssh_key)?;
    require_path(&source_rootfs)?;

    let paths = instance_paths(&cfg.install_dir, &cfg.id)?;
    let net = net_config(&paths.safe_id)?;
    let unit = format!("fastvm-microvm-{}.service", paths.safe_id);

    fs::create_dir_all(&paths.instance_dir)?;
    if paths.pid_file.exists() {
        let pid = fs::read_to_string(&paths.pid_file)?;
        if process_alive(pid.trim()) {
            return Err(format!(
                "instance {} already running with pid {}",
                paths.safe_id,
                pid.trim()
            )
            .into());
        }
    }

    let mut cpu_reservation = reserve_host_cpu_placement(&mut cfg, &paths.safe_id)?;
    timings.mark("placement");

    let _ = fs::remove_file(&paths.api_socket);
    let rootfs_clone = clone_rootfs(&source_rootfs, &paths.rootfs)?;
    timings.mark("rootfs_clone");
    configure_network(&net)?;
    timings.mark("network");

    let fc_pid = start_firecracker(&cfg, &paths, &fc_bin, &unit)?;
    fs::write(&paths.pid_file, fc_pid.to_string())?;
    wait_for_socket(&paths.api_socket)?;
    timings.mark("firecracker_start");

    fc_put(
        &paths.api_socket,
        "/logger",
        &format!(
        "{{\"log_path\":\"{}\",\"level\":\"Info\",\"show_level\":true,\"show_log_origin\":true}}",
        json_escape(path_str(&paths.log_file)?)
    ),
    )?;
    fc_put(
        &paths.api_socket,
        "/machine-config",
        &format!(
            "{{\"vcpu_count\":{},\"mem_size_mib\":{},\"smt\":false}}",
            cfg.vcpus, cfg.mem_mib
        ),
    )?;
    fc_put(&paths.api_socket, "/boot-source", &format!(
        "{{\"kernel_image_path\":\"{}\",\"boot_args\":\"console=ttyS0 reboot=k panic=1 root=/dev/vda rw\"}}",
        json_escape(path_str(&kernel)?)
    ))?;
    fc_put(&paths.api_socket, "/drives/rootfs", &format!(
        "{{\"drive_id\":\"rootfs\",\"path_on_host\":\"{}\",\"is_root_device\":true,\"is_read_only\":false}}",
        json_escape(path_str(&paths.rootfs)?)
    ))?;
    fc_put(
        &paths.api_socket,
        "/network-interfaces/net1",
        &format!(
            "{{\"iface_id\":\"net1\",\"guest_mac\":\"{}\",\"host_dev_name\":\"{}\"}}",
            net.guest_mac, net.tap_dev
        ),
    )?;
    thread::sleep(Duration::from_millis(50));
    fc_put(
        &paths.api_socket,
        "/actions",
        "{\"action_type\":\"InstanceStart\"}",
    )?;
    pin_vcpus(
        fc_pid,
        cfg.vcpus,
        cfg.host_cpus.as_deref(),
        cfg.cpu_placement_mode.as_deref(),
    )?;
    timings.mark("configure_vm");

    {
        let _lock = PlacementLock::acquire(&cfg.install_dir)?;
        write_state(
            &paths,
            &cfg,
            &net,
            fc_pid,
            &unit,
            &ssh_key,
            &source_rootfs,
            &kernel,
        )?;
        cpu_reservation.remove()?;
    }
    reconcile_firecracker_cpu_placement(&cfg.install_dir, &cfg.fc_slice, "run");
    timings.mark("state");

    if cfg.wait_for_ssh {
        wait_for_guest_ssh(&ssh_key, net.guest_ip, net.tap_ip)?;
    }
    timings.mark("ssh_ready");

    println!("ID={}", paths.safe_id);
    println!("PID={fc_pid}");
    println!("TAP={}", net.tap_dev);
    println!("GUEST_IP={}", net.guest_ip);
    println!("TEMPLATE={}", cfg.template);
    println!("ROOTFS_CLONE_METHOD={}", rootfs_clone.method.as_str());
    println!("ROOTFS_SOURCE_BYTES={}", rootfs_clone.source_bytes);
    println!("ROOTFS_DEST_BYTES={}", rootfs_clone.dest_bytes);
    if let Some(fstype) = filesystem_type_for(&cfg.install_dir) {
        println!("STORAGE_FSTYPE={fstype}");
    }
    if let Some(host_cpus) = &cfg.host_cpus {
        println!("HOST_CPUS={host_cpus}");
    }
    if let Some(mode) = &cfg.cpu_placement_mode {
        println!("CPU_PLACEMENT_MODE={mode}");
    }
    for (phase, ms) in &timings.entries {
        println!("TIME_{}_MS={ms}", phase_key(phase));
    }
    println!("TIME_TOTAL_MS={}", timings.total_ms());
    println!("SSH=ssh -i {} root@{}", ssh_key.display(), net.guest_ip);

    Ok(())
}

fn cmd_stop((install_dir, id, delete_instance): (PathBuf, String, bool)) -> Result<()> {
    ensure_root()?;
    let paths = instance_paths(&install_dir, &id)?;
    let state = read_state(&paths.state_file)?;
    cleanup_instance(&paths, &state, delete_instance)?;
    let fc_slice = state
        .get("FC_SLICE")
        .cloned()
        .unwrap_or_else(|| env::var("FC_SLICE").unwrap_or_else(|_| DEFAULT_FC_SLICE.to_string()));
    reconcile_firecracker_cpu_placement(&install_dir, &fc_slice, "stop");
    println!("stopped {}", paths.safe_id);
    Ok(())
}

fn cmd_snapshot(opts: SnapshotArgs) -> Result<()> {
    ensure_root()?;

    let paths = instance_paths(&opts.install_dir, &opts.id)?;
    let state = read_state(&paths.state_file)?;
    let rootfs = state.get("ROOTFS").ok_or("state missing ROOTFS")?;
    let rootfs_path = PathBuf::from(rootfs);
    require_path(&rootfs_path)?;

    let safe_template = sanitize_template(&opts.template)?;
    let template_dir = opts.install_dir.join("templates");
    let template_rootfs = template_dir.join(format!("{safe_template}.ext4"));
    let template_meta = template_dir.join(format!("{safe_template}.env"));
    fs::create_dir_all(&template_dir)?;
    if template_rootfs.exists() {
        return Err(format!("template already exists: {}", template_rootfs.display()).into());
    }

    request_guest_poweroff(&state);

    if let Some(pid) = state.get("PID").and_then(|s| s.parse::<u32>().ok()) {
        wait_process_exit(pid, 20, Duration::from_millis(250));
    }
    cleanup_process_only(&state)?;

    run_checked("sync", &[])?;
    snapshot_rootfs(&rootfs_path, &template_rootfs, opts.delete_instance)?;

    let meta = format!(
        "TEMPLATE={safe_template}\nCREATED_FROM={}\nCREATED_AT={}\nSOURCE_ROOTFS={}\nVCPUS={}\nMEM_MIB={}\nKERNEL={}\n",
        paths.safe_id,
        utc_timestamp(),
        rootfs_path.display(),
        state.get("VCPUS").map(String::as_str).unwrap_or(""),
        state.get("MEM_MIB").map(String::as_str).unwrap_or(""),
        state.get("KERNEL").map(String::as_str).unwrap_or(""),
    );
    fs::write(&template_meta, meta)?;

    let fc_slice = state
        .get("FC_SLICE")
        .cloned()
        .unwrap_or_else(|| env::var("FC_SLICE").unwrap_or_else(|_| DEFAULT_FC_SLICE.to_string()));
    cleanup_network_and_state(&paths, &state, opts.delete_instance)?;
    reconcile_firecracker_cpu_placement(&opts.install_dir, &fc_slice, "snapshot");

    println!("TEMPLATE={safe_template}");
    println!("ROOTFS={}", template_rootfs.display());
    println!("META={}", template_meta.display());
    Ok(())
}

fn cmd_list_templates(install_dir: PathBuf) -> Result<()> {
    println!("{:<28} {:<10} {:<10} PATH", "NAME", "KIND", "SIZE");
    for (kind, dir) in [
        ("base", install_dir.join("images")),
        ("template", install_dir.join("templates")),
    ] {
        if !dir.is_dir() {
            continue;
        }
        let mut paths: Vec<PathBuf> = fs::read_dir(&dir)?
            .filter_map(|entry| entry.ok().map(|e| e.path()))
            .filter(|path| path.extension() == Some(OsStr::new("ext4")))
            .collect();
        paths.sort();
        for path in paths {
            let name = path.file_stem().and_then(OsStr::to_str).unwrap_or("-");
            println!(
                "{:<28} {:<10} {:<10} {}",
                name,
                kind,
                du_h(&path),
                path.display()
            );
        }
    }
    Ok(())
}

fn cmd_storage_info(install_dir: PathBuf) -> Result<()> {
    ensure_root()?;
    fs::create_dir_all(&install_dir)?;
    let fstype = filesystem_type_for(&install_dir).unwrap_or_else(|| "unknown".to_string());
    let test_dir = install_dir.join(".reflink-test");
    let source = test_dir.join("source");
    let dest = test_dir.join("dest");
    fs::create_dir_all(&test_dir)?;
    fs::write(&source, b"fastvm-reflink-test")?;
    let _ = fs::remove_file(&dest);
    let reflink_supported = run_status(
        "cp",
        &["--reflink=always", path_str(&source)?, path_str(&dest)?],
    )?;
    let _ = fs::remove_dir_all(&test_dir);

    println!("INSTALL_DIR={}", install_dir.display());
    println!("STORAGE_FSTYPE={fstype}");
    println!(
        "REFLINK_SUPPORTED={}",
        if reflink_supported { "1" } else { "0" }
    );
    println!(
        "ROOTFS_CLONE_METHOD={}",
        if reflink_supported { "reflink" } else { "copy" }
    );
    Ok(())
}

fn ensure_root() -> Result<()> {
    let out = Command::new("id").arg("-u").output()?;
    if String::from_utf8_lossy(&out.stdout).trim() != "0" {
        return Err("this command must run as root".into());
    }
    Ok(())
}

fn latest_kernel(install_dir: &Path) -> Result<PathBuf> {
    let image_dir = install_dir.join("images");
    let mut kernels: Vec<PathBuf> = fs::read_dir(&image_dir)?
        .filter_map(|entry| entry.ok().map(|e| e.path()))
        .filter(|path| {
            path.file_name()
                .and_then(OsStr::to_str)
                .map(|name| name.starts_with("vmlinux-"))
                .unwrap_or(false)
        })
        .collect();
    kernels.sort();
    kernels
        .pop()
        .ok_or_else(|| format!("no vmlinux-* kernel found in {}", image_dir.display()).into())
}

fn template_path(cfg: &RunConfig) -> Result<PathBuf> {
    if let Some(path) = &cfg.rootfs_image {
        return Ok(path.clone());
    }
    if cfg.template.contains('/') {
        return Ok(PathBuf::from(&cfg.template));
    }
    let template_path = cfg
        .install_dir
        .join("templates")
        .join(format!("{}.ext4", cfg.template));
    if template_path.exists() {
        return Ok(template_path);
    }
    let image_path = cfg
        .install_dir
        .join("images")
        .join(format!("{}.ext4", cfg.template));
    Ok(image_path)
}

fn require_path(path: &Path) -> Result<()> {
    if !path.exists() {
        return Err(format!("missing required asset: {}", path.display()).into());
    }
    Ok(())
}

fn instance_paths(install_dir: &Path, id: &str) -> Result<InstancePaths> {
    let safe_id = sanitize_id(id)?;
    let instance_dir = install_dir.join("instances").join(&safe_id);
    Ok(InstancePaths {
        safe_id,
        api_socket: instance_dir.join("firecracker.socket"),
        log_file: instance_dir.join("firecracker.log"),
        rootfs: instance_dir.join("rootfs.ext4"),
        pid_file: instance_dir.join("firecracker.pid"),
        state_file: instance_dir.join("state.env"),
        stdout_log: instance_dir.join("stdout.log"),
        stderr_log: instance_dir.join("stderr.log"),
        instance_dir,
    })
}

fn sanitize_id(id: &str) -> Result<String> {
    let out: String = id
        .chars()
        .filter(|c| c.is_ascii_alphanumeric() || *c == '-')
        .take(32)
        .collect();
    if out.is_empty() {
        return Err(format!("invalid ID: {id}").into());
    }
    Ok(out)
}

fn sanitize_template(template: &str) -> Result<String> {
    let out: String = template
        .chars()
        .filter(|c| c.is_ascii_alphanumeric() || *c == '-')
        .take(48)
        .collect();
    if out.is_empty() {
        return Err(format!("invalid template: {template}").into());
    }
    Ok(out)
}

fn net_config(safe_id: &str) -> Result<NetConfig> {
    let tap_dev = tap_dev_for_id(safe_id);
    let net_octet = ((fnv1a32(safe_id.as_bytes()) % 200) + 16) as u8;
    let tap_ip = Ipv4Addr::new(192, 168, net_octet, 1);
    let guest_ip = Ipv4Addr::new(192, 168, net_octet, 2);
    let guest_mac = format!("06:00:c0:a8:{net_octet:02x}:02");
    let host_iface = default_route_iface()?;
    Ok(NetConfig {
        tap_dev,
        tap_ip,
        guest_ip,
        guest_mac,
        host_iface,
    })
}

fn tap_dev_for_id(safe_id: &str) -> String {
    let mut prefix: String = safe_id
        .chars()
        .filter(|c| c.is_ascii_alphanumeric())
        .take(7)
        .collect();
    if prefix.is_empty() {
        prefix.push_str("vm");
    }
    let hash = fnv1a32(safe_id.as_bytes()) & 0x00ff_ffff;
    format!("fc{prefix}{hash:06x}")
}

fn fnv1a32(bytes: &[u8]) -> u32 {
    let mut hash = 0x811c9dc5u32;
    for b in bytes {
        hash ^= *b as u32;
        hash = hash.wrapping_mul(0x01000193);
    }
    hash
}

fn default_route_iface() -> Result<String> {
    let out = command_output("ip", &["route", "show", "default"])?;
    let words: Vec<&str> = out.split_whitespace().collect();
    for pair in words.windows(2) {
        if pair[0] == "dev" {
            return Ok(pair[1].to_string());
        }
    }
    Err("could not determine default route interface".into())
}

fn sparse_copy(source: &Path, dest: &Path) -> Result<()> {
    run_checked(
        "cp",
        &[
            "--reflink=auto",
            "--sparse=always",
            path_str(source)?,
            path_str(dest)?,
        ],
    )
}

fn clone_rootfs(source: &Path, dest: &Path) -> Result<RootfsCloneResult> {
    let source_bytes = fs::metadata(source)?.len();
    if run_status(
        "cp",
        &["--reflink=always", path_str(source)?, path_str(dest)?],
    )? {
        return Ok(RootfsCloneResult {
            method: RootfsCloneMethod::Reflink,
            source_bytes,
            dest_bytes: fs::metadata(dest)?.len(),
        });
    }

    let _ = fs::remove_file(dest);
    run_checked(
        "cp",
        &[
            "--reflink=never",
            "--sparse=always",
            path_str(source)?,
            path_str(dest)?,
        ],
    )?;
    Ok(RootfsCloneResult {
        method: RootfsCloneMethod::Copy,
        source_bytes,
        dest_bytes: fs::metadata(dest)?.len(),
    })
}

fn snapshot_rootfs(source: &Path, dest: &Path, consume_source: bool) -> Result<()> {
    if !consume_source {
        return sparse_copy(source, dest);
    }

    match fs::rename(source, dest) {
        Ok(()) => Ok(()),
        Err(err) if err.raw_os_error() == Some(18) => sparse_copy(source, dest),
        Err(err) => Err(format!(
            "move snapshot rootfs {} to {}: {err}",
            source.display(),
            dest.display()
        )
        .into()),
    }
}

fn request_guest_poweroff(state: &BTreeMap<String, String>) {
    if let (Some(guest_ip), Some(ssh_key)) = (state.get("GUEST_IP"), state.get("SSH_KEY")) {
        let guest = format!("root@{guest_ip}");
        let command =
            "sync; nohup sh -c 'sync; systemctl poweroff --force --force || poweroff -f || halt -f' >/dev/null 2>&1 &";
        let _ = run_status(
            "ssh",
            &[
                "-o",
                "StrictHostKeyChecking=no",
                "-o",
                "UserKnownHostsFile=/dev/null",
                "-o",
                "ConnectTimeout=2",
                "-i",
                ssh_key,
                &guest,
                command,
            ],
        );
    }
}

fn configure_network(net: &NetConfig) -> Result<()> {
    let _ = run_status("ip", &["link", "del", &net.tap_dev]);
    run_checked("ip", &["tuntap", "add", "dev", &net.tap_dev, "mode", "tap"])?;
    run_checked(
        "ip",
        &[
            "addr",
            "add",
            &format!("{}/30", net.tap_ip),
            "dev",
            &net.tap_dev,
        ],
    )?;
    run_checked("ip", &["link", "set", "dev", &net.tap_dev, "up"])?;
    fs::write("/proc/sys/net/ipv4/ip_forward", "1\n")?;

    ensure_shared_firecracker_firewall(&net.host_iface)?;
    Ok(())
}

fn ensure_shared_firecracker_firewall(host_iface: &str) -> Result<()> {
    iptables_ensure(&[
        "FORWARD",
        "-i",
        FIRECRACKER_TAP_IFACE_PATTERN,
        "-j",
        "ACCEPT",
    ])?;
    iptables_ensure(&[
        "FORWARD",
        "-o",
        FIRECRACKER_TAP_IFACE_PATTERN,
        "-m",
        "conntrack",
        "--ctstate",
        "RELATED,ESTABLISHED",
        "-j",
        "ACCEPT",
    ])?;
    iptables_nat_ensure(&[
        "POSTROUTING",
        "-s",
        FIRECRACKER_GUEST_CIDR,
        "-o",
        host_iface,
        "-j",
        "MASQUERADE",
    ])?;
    Ok(())
}

fn iptables_bin() -> &'static str {
    if Path::new("/usr/sbin/iptables").exists() {
        "/usr/sbin/iptables"
    } else {
        "iptables"
    }
}

fn iptables_ensure(args: &[&str]) -> Result<()> {
    let mut check = vec!["-C"];
    check.extend_from_slice(args);
    if run_status(iptables_bin(), &check)? {
        return Ok(());
    }
    let mut add = vec!["-A"];
    add.extend_from_slice(args);
    run_checked(iptables_bin(), &add)
}

fn iptables_nat_ensure(args: &[&str]) -> Result<()> {
    let mut check = vec!["-t", "nat", "-C"];
    check.extend_from_slice(args);
    if run_status(iptables_bin(), &check)? {
        return Ok(());
    }
    let mut add = vec!["-t", "nat", "-A"];
    add.extend_from_slice(args);
    run_checked(iptables_bin(), &add)
}

fn start_firecracker(
    cfg: &RunConfig,
    paths: &InstancePaths,
    fc_bin: &Path,
    unit: &str,
) -> Result<u32> {
    if command_exists("systemd-run") && Path::new("/run/systemd/system").is_dir() {
        let mut args = vec![
            "--unit".to_string(),
            unit.to_string(),
            "--slice".to_string(),
            cfg.fc_slice.clone(),
            "--collect".to_string(),
        ];
        if let Some(cpus) = &cfg.host_cpus {
            args.push("--property".to_string());
            args.push(format!("AllowedCPUs={cpus}"));
        }
        args.push("--property".to_string());
        args.push(format!(
            "StandardOutput=append:{}",
            paths.stdout_log.display()
        ));
        args.push("--property".to_string());
        args.push(format!(
            "StandardError=append:{}",
            paths.stderr_log.display()
        ));
        args.push(path_str(fc_bin)?.to_string());
        args.push("--api-sock".to_string());
        args.push(path_str(&paths.api_socket)?.to_string());
        args.push("--enable-pci".to_string());
        run_checked_owned("systemd-run", &args)?;

        for _ in 0..100 {
            let pid = command_output("systemctl", &["show", unit, "-p", "MainPID", "--value"])
                .unwrap_or_default();
            if let Ok(n) = pid.trim().parse::<u32>() {
                if n != 0 {
                    return Ok(n);
                }
            }
            thread::sleep(Duration::from_millis(50));
        }
        return Err("Firecracker systemd unit did not report a MainPID".into());
    }

    let stdout = OpenOptions::new()
        .create(true)
        .append(true)
        .open(&paths.stdout_log)?;
    let stderr = OpenOptions::new()
        .create(true)
        .append(true)
        .open(&paths.stderr_log)?;
    let child = Command::new(fc_bin)
        .arg("--api-sock")
        .arg(&paths.api_socket)
        .arg("--enable-pci")
        .stdout(Stdio::from(stdout))
        .stderr(Stdio::from(stderr))
        .spawn()?;
    Ok(child.id())
}

fn wait_for_socket(path: &Path) -> Result<()> {
    for _ in 0..100 {
        if path.exists() {
            return Ok(());
        }
        thread::sleep(Duration::from_millis(50));
    }
    Err(format!("Firecracker API socket did not appear: {}", path.display()).into())
}

fn fc_put(socket: &Path, endpoint: &str, body: &str) -> Result<()> {
    let mut stream = UnixStream::connect(socket)?;
    stream.set_read_timeout(Some(Duration::from_secs(2)))?;
    let request = format!(
        "PUT {endpoint} HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.as_bytes().len()
    );
    stream.write_all(request.as_bytes())?;

    let mut response = Vec::new();
    let mut buf = [0u8; 4096];
    loop {
        match stream.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => {
                response.extend_from_slice(&buf[..n]);
                if http_response_complete(&response) {
                    break;
                }
            }
            Err(err)
                if err.kind() == std::io::ErrorKind::WouldBlock
                    || err.kind() == std::io::ErrorKind::TimedOut =>
            {
                break
            }
            Err(err)
                if err.kind() == std::io::ErrorKind::ConnectionReset && !response.is_empty() =>
            {
                break
            }
            Err(err) => return Err(err.into()),
        }
    }

    let response = String::from_utf8_lossy(&response);
    let status = response.lines().next().unwrap_or("");
    if status.contains(" 2") {
        Ok(())
    } else {
        Err(format!("Firecracker API {endpoint} failed: {status} {response}").into())
    }
}

fn http_response_complete(response: &[u8]) -> bool {
    let Some(header_end) = find_header_end(response) else {
        return false;
    };
    let headers = String::from_utf8_lossy(&response[..header_end]);
    let content_length = headers.lines().find_map(|line| {
        let (name, value) = line.split_once(':')?;
        if name.eq_ignore_ascii_case("content-length") {
            value.trim().parse::<usize>().ok()
        } else {
            None
        }
    });
    match content_length {
        Some(length) => response.len() >= header_end + 4 + length,
        None => true,
    }
}

fn find_header_end(response: &[u8]) -> Option<usize> {
    response.windows(4).position(|window| window == b"\r\n\r\n")
}

fn reserve_host_cpu_placement(cfg: &mut RunConfig, safe_id: &str) -> Result<CpuReservationGuard> {
    let _lock = PlacementLock::acquire(&cfg.install_dir)?;
    remove_expired_cpu_reservations(&cfg.install_dir)?;
    if active_cpu_reservations(&cfg.install_dir)?
        .iter()
        .any(|claim| claim.id == safe_id)
    {
        return Err(format!("instance {safe_id} is already being created").into());
    }

    resolve_host_cpu_placement(cfg, safe_id)?;
    write_cpu_reservation(&cfg.install_dir, safe_id, cfg)
}

fn resolve_host_cpu_placement(cfg: &mut RunConfig, safe_id: &str) -> Result<()> {
    if cfg.host_cpus_explicit {
        let host_cpus = cfg
            .host_cpus
            .as_deref()
            .ok_or("HOST_CPUS was marked explicit but no CPU list was provided")?;
        let cpus = expand_cpu_list(host_cpus)?;
        if cpus.is_empty() {
            return Err("HOST_CPUS resolved to an empty CPU list".into());
        }
        cfg.cpu_placement_mode = Some(CPU_PLACEMENT_MANUAL.to_string());
        cfg.cpu_placement_total_requested = Some(cfg.vcpus);
        cfg.cpu_placement_allowed_count = Some(cpus.len());
        return Ok(());
    }

    let mut allowed = read_slice_cpus(&cfg.fc_slice)?;
    let claims = cpu_placement_claims(&cfg.install_dir)?;
    reserve_manual_cpu_placements(&mut allowed, &claims)?;
    let mut requests: Vec<CpuRequest> = claims
        .into_iter()
        .filter(|claim| !claim.is_manual_cpu_placement())
        .filter(|claim| claim.id != safe_id)
        .map(|claim| CpuRequest {
            id: claim.id,
            vcpus: claim.vcpus,
        })
        .collect();
    requests.push(CpuRequest {
        id: safe_id.to_string(),
        vcpus: cfg.vcpus,
    });

    let placements = calculate_cpu_placements(&requests, &allowed);
    let placement = placements
        .get(safe_id)
        .ok_or_else(|| format!("no CPU placement calculated for {safe_id}"))?;
    let affinity = placement.affinity();
    if affinity.is_empty() {
        return Err(format!("CPU placement for {safe_id} resolved to an empty CPU list").into());
    }

    cfg.host_cpus = Some(affinity);
    cfg.cpu_placement_mode = Some(placement.mode.to_string());
    cfg.cpu_placement_total_requested = Some(placement.total_requested);
    cfg.cpu_placement_allowed_count = Some(placement.allowed_count);
    Ok(())
}

fn reconcile_firecracker_cpu_placement(install_dir: &Path, fc_slice: &str, reason: &str) {
    let allowed = match read_slice_cpus(fc_slice) {
        Ok(allowed) => allowed,
        Err(err) => {
            eprintln!("warning: failed to read CPU placement set for {reason}: {err}");
            return;
        }
    };
    let instances = match running_firecracker_instances(install_dir) {
        Ok(instances) => instances,
        Err(err) => {
            eprintln!("warning: failed to list Firecracker instances for {reason}: {err}");
            return;
        }
    };
    if instances.is_empty() {
        return;
    }

    let mut allowed = allowed;
    let claims: Vec<CpuPlacementClaim> = instances
        .iter()
        .map(running_instance_to_cpu_claim)
        .collect();
    if let Err(err) = reserve_manual_cpu_placements(&mut allowed, &claims) {
        eprintln!("warning: failed to reserve manual CPU placements for {reason}: {err}");
        return;
    }

    let requests: Vec<CpuRequest> = instances
        .iter()
        .filter(|instance| !instance.is_manual_cpu_placement())
        .map(|instance| CpuRequest {
            id: instance.id.clone(),
            vcpus: instance.vcpus,
        })
        .collect();
    let placements = calculate_cpu_placements(&requests, &allowed);

    for instance in instances {
        if instance.is_manual_cpu_placement() {
            continue;
        }
        let Some(placement) = placements.get(&instance.id) else {
            continue;
        };
        if let Err(err) = apply_cpu_placement(&instance, placement) {
            eprintln!(
                "warning: failed to reconcile CPU placement for {} after {reason}: {err}",
                instance.id
            );
        }
    }
}

fn apply_cpu_placement(
    instance: &RunningFirecrackerInstance,
    placement: &CpuPlacement,
) -> Result<()> {
    let affinity = placement.affinity();
    if affinity.is_empty() {
        return Ok(());
    }

    if let Some(unit) = instance.unit.as_deref() {
        if command_exists("systemctl") {
            let property = format!("AllowedCPUs={affinity}");
            if !run_status("systemctl", &["set-property", unit, &property])? {
                eprintln!(
                    "warning: systemctl set-property failed for {} ({unit})",
                    instance.id
                );
            }
        }
    } else {
        run_checked("taskset", &["-pc", &affinity, &instance.pid.to_string()])?;
    }

    pin_vcpus(
        instance.pid,
        instance.vcpus,
        Some(&affinity),
        Some(placement.mode),
    )?;
    update_state_cpu_placement(&instance.state_file, placement)?;
    Ok(())
}

fn running_firecracker_instances(install_dir: &Path) -> Result<Vec<RunningFirecrackerInstance>> {
    let instances_dir = install_dir.join("instances");
    if !instances_dir.is_dir() {
        return Ok(Vec::new());
    }

    let mut instances = Vec::new();
    for entry in fs::read_dir(&instances_dir)? {
        let entry = entry?;
        if !entry.file_type()?.is_dir() {
            continue;
        }

        let state_file = entry.path().join("state.env");
        let state = match read_state(&state_file) {
            Ok(state) => state,
            Err(_) => continue,
        };
        let Some(pid) = state.get("PID").and_then(|pid| pid.parse::<u32>().ok()) else {
            continue;
        };
        if !process_alive(&pid.to_string()) {
            continue;
        }

        let id = state
            .get("ID")
            .cloned()
            .or_else(|| entry.file_name().to_str().map(|name| name.to_string()))
            .unwrap_or_default();
        if id.is_empty() {
            continue;
        }
        let vcpus = state
            .get("VCPUS")
            .and_then(|vcpus| vcpus.parse::<u32>().ok())
            .filter(|vcpus| *vcpus > 0)
            .unwrap_or(1);
        let unit = state.get("UNIT").filter(|unit| !unit.is_empty()).cloned();
        let host_cpus = state
            .get("HOST_CPUS")
            .filter(|host_cpus| !host_cpus.is_empty())
            .cloned();
        let cpu_placement_mode = state
            .get("CPU_PLACEMENT_MODE")
            .filter(|mode| !mode.is_empty())
            .cloned();

        instances.push(RunningFirecrackerInstance {
            id,
            vcpus,
            pid,
            unit,
            host_cpus,
            cpu_placement_mode,
            state_file,
        });
    }

    Ok(instances)
}

fn running_instance_to_cpu_claim(instance: &RunningFirecrackerInstance) -> CpuPlacementClaim {
    CpuPlacementClaim {
        id: instance.id.clone(),
        vcpus: instance.vcpus,
        host_cpus: instance.host_cpus.clone(),
        cpu_placement_mode: instance.cpu_placement_mode.clone(),
    }
}

fn cpu_placement_claims(install_dir: &Path) -> Result<Vec<CpuPlacementClaim>> {
    let mut claims: Vec<CpuPlacementClaim> = running_firecracker_instances(install_dir)?
        .iter()
        .map(running_instance_to_cpu_claim)
        .collect();
    claims.extend(active_cpu_reservations(install_dir)?);
    Ok(claims)
}

fn cpu_reservations_dir(install_dir: &Path) -> PathBuf {
    install_dir.join("placement-reservations")
}

fn cpu_reservation_path(install_dir: &Path, safe_id: &str) -> PathBuf {
    cpu_reservations_dir(install_dir).join(format!("{safe_id}.env"))
}

fn write_cpu_reservation(
    install_dir: &Path,
    safe_id: &str,
    cfg: &RunConfig,
) -> Result<CpuReservationGuard> {
    let dir = cpu_reservations_dir(install_dir);
    fs::create_dir_all(&dir)?;
    let path = cpu_reservation_path(install_dir, safe_id);
    if path.exists() {
        return Err(format!("CPU reservation already exists: {}", path.display()).into());
    }

    let now = unix_timestamp_secs();
    let mut file = File::create(&path)?;
    writeln!(file, "ID={safe_id}")?;
    writeln!(file, "VCPUS={}", cfg.vcpus)?;
    writeln!(file, "HOST_CPUS={}", cfg.host_cpus.as_deref().unwrap_or(""))?;
    writeln!(
        file,
        "CPU_PLACEMENT_MODE={}",
        cfg.cpu_placement_mode.as_deref().unwrap_or("")
    )?;
    writeln!(file, "CREATED_AT_EPOCH={now}")?;

    Ok(CpuReservationGuard { path: Some(path) })
}

fn active_cpu_reservations(install_dir: &Path) -> Result<Vec<CpuPlacementClaim>> {
    let dir = cpu_reservations_dir(install_dir);
    if !dir.is_dir() {
        return Ok(Vec::new());
    }

    let mut claims = Vec::new();
    for entry in fs::read_dir(&dir)? {
        let entry = entry?;
        if !entry.file_type()?.is_file() {
            continue;
        }
        let path = entry.path();
        let state = match read_state(&path) {
            Ok(state) => state,
            Err(_) => continue,
        };
        if cpu_reservation_expired(&state) {
            let _ = fs::remove_file(&path);
            continue;
        }
        let Some(id) = state.get("ID").filter(|id| !id.is_empty()).cloned() else {
            continue;
        };
        let vcpus = state
            .get("VCPUS")
            .and_then(|vcpus| vcpus.parse::<u32>().ok())
            .filter(|vcpus| *vcpus > 0)
            .unwrap_or(1);
        let host_cpus = state
            .get("HOST_CPUS")
            .filter(|host_cpus| !host_cpus.is_empty())
            .cloned();
        let cpu_placement_mode = state
            .get("CPU_PLACEMENT_MODE")
            .filter(|mode| !mode.is_empty())
            .cloned();

        claims.push(CpuPlacementClaim {
            id,
            vcpus,
            host_cpus,
            cpu_placement_mode,
        });
    }

    Ok(claims)
}

fn remove_expired_cpu_reservations(install_dir: &Path) -> Result<()> {
    let dir = cpu_reservations_dir(install_dir);
    if !dir.is_dir() {
        return Ok(());
    }

    for entry in fs::read_dir(&dir)? {
        let entry = entry?;
        if !entry.file_type()?.is_file() {
            continue;
        }
        let path = entry.path();
        let Ok(state) = read_state(&path) else {
            continue;
        };
        if cpu_reservation_expired(&state) {
            let _ = fs::remove_file(path);
        }
    }
    Ok(())
}

fn cpu_reservation_expired(state: &BTreeMap<String, String>) -> bool {
    let Some(created) = state
        .get("CREATED_AT_EPOCH")
        .and_then(|created| created.parse::<u64>().ok())
    else {
        return true;
    };
    unix_timestamp_secs().saturating_sub(created) > CPU_RESERVATION_TTL_SECS
}

fn reserve_manual_cpu_placements(
    allowed: &mut Vec<u32>,
    claims: &[CpuPlacementClaim],
) -> Result<()> {
    for claim in claims
        .iter()
        .filter(|claim| claim.is_manual_cpu_placement())
    {
        let Some(host_cpus) = claim.host_cpus.as_deref() else {
            continue;
        };
        let reserved = expand_cpu_list(host_cpus)?;
        allowed.retain(|cpu| !reserved.contains(cpu));
    }
    if allowed.is_empty() {
        return Err("manual Firecracker CPU placements reserved the entire slice CPU set".into());
    }
    Ok(())
}

fn read_slice_cpus(slice: &str) -> Result<Vec<u32>> {
    let path = slice_cpuset_path(slice)?;
    let data =
        fs::read_to_string(&path).map_err(|err| format!("read {}: {err}", path.display()))?;
    let cpus = expand_cpu_list(&data).map_err(|err| format!("parse {}: {err}", path.display()))?;
    if cpus.is_empty() {
        return Err(format!("empty CPU list in {}", path.display()).into());
    }
    Ok(cpus)
}

fn slice_cpuset_path(slice: &str) -> Result<PathBuf> {
    let slice = slice.trim();
    if slice.is_empty() {
        return Err("systemd slice name is empty".into());
    }
    if slice.contains('/') || slice.contains('\0') {
        return Err(format!("invalid systemd slice name: {slice}").into());
    }
    Ok(Path::new("/sys/fs/cgroup")
        .join(slice)
        .join("cpuset.cpus.effective"))
}

fn calculate_cpu_placements(
    requests: &[CpuRequest],
    allowed: &[u32],
) -> BTreeMap<String, CpuPlacement> {
    let mut placements = BTreeMap::new();
    if requests.is_empty() || allowed.is_empty() {
        return placements;
    }

    let mut reqs = requests.to_vec();
    reqs.sort_by(|a, b| a.id.cmp(&b.id));

    let total_requested = reqs
        .iter_mut()
        .map(|req| {
            if req.vcpus == 0 {
                req.vcpus = 1;
            }
            req.vcpus
        })
        .sum::<u32>();

    let mode = if total_requested as usize > allowed.len() {
        CPU_PLACEMENT_SHARED
    } else {
        CPU_PLACEMENT_DEDICATED
    };

    let mut cursor = 0usize;
    for req in reqs {
        let count = (req.vcpus as usize).min(allowed.len());
        let host_cpus = cpu_window(allowed, cursor, count);
        placements.insert(
            req.id.clone(),
            CpuPlacement {
                id: req.id,
                vcpus: req.vcpus,
                host_cpus,
                mode,
                total_requested,
                allowed_count: allowed.len(),
            },
        );

        cursor += count;
        if mode == CPU_PLACEMENT_SHARED {
            cursor %= allowed.len();
        }
    }

    placements
}

fn cpu_window(allowed: &[u32], start: usize, count: usize) -> Vec<u32> {
    if allowed.is_empty() || count == 0 {
        return Vec::new();
    }
    let count = count.min(allowed.len());
    let mut out = Vec::with_capacity(count);
    for i in 0..count {
        out.push(allowed[(start + i) % allowed.len()]);
    }
    out
}

fn pin_vcpus(
    pid: u32,
    expected_vcpus: u32,
    host_cpus: Option<&str>,
    placement_mode: Option<&str>,
) -> Result<()> {
    let Some(host_cpus) = host_cpus else {
        return Ok(());
    };
    let cpus = expand_cpu_list(host_cpus)?;
    if cpus.is_empty() {
        return Ok(());
    }

    for _ in 0..100 {
        if list_vcpu_threads(pid)?.len() >= expected_vcpus as usize {
            break;
        }
        thread::sleep(Duration::from_millis(50));
    }

    let threads = list_vcpu_threads(pid)?;
    if threads.is_empty() && expected_vcpus > 0 {
        return Err(format!("no Firecracker vCPU threads found for pid {pid}").into());
    }
    for (idx, (_vcpu, tid)) in threads.iter().enumerate() {
        let target = if placement_mode == Some(CPU_PLACEMENT_SHARED) {
            host_cpus.to_string()
        } else {
            cpus[idx % cpus.len()].to_string()
        };
        run_checked("taskset", &["-pc", &target, &tid.to_string()])?;
    }
    Ok(())
}

fn expand_cpu_list(list: &str) -> Result<Vec<u32>> {
    let mut cpus = Vec::new();
    for part in list.split(',').filter(|p| !p.trim().is_empty()) {
        let part = part.trim();
        if let Some((start, end)) = part.split_once('-') {
            let start: u32 = start.parse()?;
            let end: u32 = end.parse()?;
            if end < start {
                return Err(format!("descending CPU range: {part}").into());
            }
            cpus.extend(start..=end);
        } else {
            cpus.push(part.parse()?);
        }
    }
    Ok(cpus)
}

fn format_cpu_list(cpus: &[u32]) -> String {
    if cpus.is_empty() {
        return String::new();
    }

    let mut parts = Vec::new();
    let mut start = cpus[0];
    let mut prev = cpus[0];
    for cpu in cpus.iter().copied().skip(1) {
        if cpu == prev + 1 {
            prev = cpu;
            continue;
        }
        if start == prev {
            parts.push(start.to_string());
        } else {
            parts.push(format!("{start}-{prev}"));
        }
        start = cpu;
        prev = cpu;
    }

    if start == prev {
        parts.push(start.to_string());
    } else {
        parts.push(format!("{start}-{prev}"));
    }
    parts.join(",")
}

fn list_vcpu_threads(pid: u32) -> Result<Vec<(u32, u32)>> {
    let task_dir = PathBuf::from(format!("/proc/{pid}/task"));
    let mut threads = Vec::new();
    if !task_dir.is_dir() {
        return Ok(threads);
    }
    for entry in fs::read_dir(task_dir)? {
        let entry = entry?;
        let tid: u32 = match entry.file_name().to_string_lossy().parse() {
            Ok(tid) => tid,
            Err(_) => continue,
        };
        let comm = fs::read_to_string(entry.path().join("comm")).unwrap_or_default();
        let comm = comm.trim();
        if let Some(rest) = comm.strip_prefix("fc_vcpu ") {
            if let Ok(index) = rest.parse::<u32>() {
                threads.push((index, tid));
            }
        }
    }
    threads.sort_by_key(|(index, _)| *index);
    Ok(threads)
}

fn write_state(
    paths: &InstancePaths,
    cfg: &RunConfig,
    net: &NetConfig,
    pid: u32,
    unit: &str,
    ssh_key: &Path,
    source_rootfs: &Path,
    kernel: &Path,
) -> Result<()> {
    let mut file = File::create(&paths.state_file)?;
    writeln!(file, "ID={}", paths.safe_id)?;
    writeln!(file, "PID={pid}")?;
    writeln!(file, "TAP_DEV={}", net.tap_dev)?;
    writeln!(file, "TAP_IP={}", net.tap_ip)?;
    writeln!(file, "GUEST_IP={}", net.guest_ip)?;
    writeln!(file, "GUEST_MAC={}", net.guest_mac)?;
    writeln!(file, "API_SOCKET={}", paths.api_socket.display())?;
    writeln!(file, "ROOTFS={}", paths.rootfs.display())?;
    writeln!(file, "SOURCE_ROOTFS={}", source_rootfs.display())?;
    writeln!(file, "TEMPLATE={}", cfg.template)?;
    writeln!(file, "VCPUS={}", cfg.vcpus)?;
    writeln!(file, "MEM_MIB={}", cfg.mem_mib)?;
    writeln!(file, "SSH_KEY={}", ssh_key.display())?;
    writeln!(file, "UNIT={unit}")?;
    writeln!(file, "FC_SLICE={}", cfg.fc_slice)?;
    writeln!(file, "HOST_CPUS={}", cfg.host_cpus.as_deref().unwrap_or(""))?;
    writeln!(
        file,
        "CPU_PLACEMENT_MODE={}",
        cfg.cpu_placement_mode.as_deref().unwrap_or("")
    )?;
    writeln!(
        file,
        "CPU_PLACEMENT_TOTAL_REQUESTED={}",
        cfg.cpu_placement_total_requested
            .map(|v| v.to_string())
            .unwrap_or_default()
    )?;
    writeln!(
        file,
        "CPU_PLACEMENT_ALLOWED_COUNT={}",
        cfg.cpu_placement_allowed_count
            .map(|v| v.to_string())
            .unwrap_or_default()
    )?;
    writeln!(file, "HOST_IFACE={}", net.host_iface)?;
    writeln!(file, "FIREWALL_MODE=shared")?;
    writeln!(
        file,
        "FIRECRACKER_TAP_MATCH={FIRECRACKER_TAP_IFACE_PATTERN}"
    )?;
    writeln!(file, "FIRECRACKER_GUEST_CIDR={FIRECRACKER_GUEST_CIDR}")?;
    writeln!(file, "KERNEL={}", kernel.display())?;
    Ok(())
}

fn update_state_cpu_placement(state_file: &Path, placement: &CpuPlacement) -> Result<()> {
    let mut updates = BTreeMap::new();
    updates.insert("HOST_CPUS", placement.affinity());
    updates.insert("CPU_PLACEMENT_MODE", placement.mode.to_string());
    updates.insert(
        "CPU_PLACEMENT_TOTAL_REQUESTED",
        placement.total_requested.to_string(),
    );
    updates.insert(
        "CPU_PLACEMENT_ALLOWED_COUNT",
        placement.allowed_count.to_string(),
    );

    let data = fs::read_to_string(state_file)?;
    let mut seen = BTreeMap::new();
    let mut out = String::new();
    for line in data.lines() {
        if let Some((key, _value)) = line.split_once('=') {
            if let Some(value) = updates.get(key) {
                out.push_str(key);
                out.push('=');
                out.push_str(value);
                out.push('\n');
                seen.insert(key.to_string(), true);
                continue;
            }
        }
        out.push_str(line);
        out.push('\n');
    }

    for (key, value) in updates {
        if !seen.contains_key(key) {
            out.push_str(key);
            out.push('=');
            out.push_str(&value);
            out.push('\n');
        }
    }

    fs::write(state_file, out)?;
    Ok(())
}

fn read_state(path: &Path) -> Result<BTreeMap<String, String>> {
    let data = fs::read_to_string(path)
        .map_err(|err| format!("state not found: {}: {err}", path.display()))?;
    let mut out = BTreeMap::new();
    for line in data.lines() {
        if line.trim().is_empty() || line.starts_with('#') {
            continue;
        }
        if let Some((key, value)) = line.split_once('=') {
            out.insert(key.to_string(), value.to_string());
        }
    }
    Ok(out)
}

fn wait_for_guest_ssh(ssh_key: &Path, guest_ip: Ipv4Addr, tap_ip: Ipv4Addr) -> Result<()> {
    let key = path_str(ssh_key)?;
    let guest = format!("root@{guest_ip}");
    for _ in 0..60 {
        if run_status(
            "ssh",
            &[
                "-o",
                "StrictHostKeyChecking=no",
                "-o",
                "UserKnownHostsFile=/dev/null",
                "-o",
                "ConnectTimeout=1",
                "-i",
                key,
                &guest,
                "true",
            ],
        )? {
            let setup = format!(
                "ip route replace default via {tap_ip} dev eth0; printf 'nameserver 10.255.0.100\\nnameserver 8.8.8.8\\n' > /etc/resolv.conf"
            );
            let _ = run_status(
                "ssh",
                &[
                    "-o",
                    "StrictHostKeyChecking=no",
                    "-o",
                    "UserKnownHostsFile=/dev/null",
                    "-i",
                    key,
                    &guest,
                    &setup,
                ],
            );
            return Ok(());
        }
        thread::sleep(Duration::from_secs(1));
    }
    Ok(())
}

fn cleanup_instance(
    paths: &InstancePaths,
    state: &BTreeMap<String, String>,
    delete_instance: bool,
) -> Result<()> {
    cleanup_process_only(state)?;
    cleanup_network_and_state(paths, state, delete_instance)
}

fn cleanup_process_only(state: &BTreeMap<String, String>) -> Result<()> {
    if let Some(unit) = state.get("UNIT").filter(|u| !u.is_empty()) {
        let _ = run_status("systemctl", &["stop", unit]);
    }
    if let Some(pid) = state.get("PID").and_then(|s| s.parse::<u32>().ok()) {
        if process_alive(&pid.to_string()) {
            let _ = run_status("kill", &[&pid.to_string()]);
            wait_process_exit(pid, 50, Duration::from_millis(100));
            if process_alive(&pid.to_string()) {
                let _ = run_status("kill", &["-9", &pid.to_string()]);
            }
        }
    }
    Ok(())
}

fn cleanup_network_and_state(
    paths: &InstancePaths,
    state: &BTreeMap<String, String>,
    delete_instance: bool,
) -> Result<()> {
    if let Some(tap_dev) = state.get("TAP_DEV").filter(|v| !v.is_empty()) {
        // Older releases installed per-VM firewall rules. Remove those when
        // present, but keep the shared fc+/CIDR rules for other running VMs.
        if let (Some(guest_ip), Some(host_iface)) = (state.get("GUEST_IP"), state.get("HOST_IFACE"))
        {
            let _ = run_status(
                iptables_bin(),
                &[
                    "-t",
                    "nat",
                    "-D",
                    "POSTROUTING",
                    "-s",
                    &format!("{guest_ip}/32"),
                    "-o",
                    host_iface,
                    "-j",
                    "MASQUERADE",
                ],
            );
        }
        let _ = run_status(
            iptables_bin(),
            &["-D", "FORWARD", "-i", tap_dev, "-j", "ACCEPT"],
        );
        let _ = run_status(
            iptables_bin(),
            &[
                "-D",
                "FORWARD",
                "-o",
                tap_dev,
                "-m",
                "conntrack",
                "--ctstate",
                "RELATED,ESTABLISHED",
                "-j",
                "ACCEPT",
            ],
        );
        let _ = run_status("ip", &["link", "del", tap_dev]);
    }
    if let Some(api_socket) = state.get("API_SOCKET").filter(|v| !v.is_empty()) {
        let _ = fs::remove_file(api_socket);
    }
    let _ = fs::remove_file(&paths.pid_file);
    let _ = fs::remove_file(&paths.state_file);
    if delete_instance {
        fs::remove_dir_all(&paths.instance_dir)?;
    }
    Ok(())
}

fn wait_process_exit(pid: u32, attempts: usize, delay: Duration) {
    for _ in 0..attempts {
        if !process_alive(&pid.to_string()) {
            return;
        }
        thread::sleep(delay);
    }
}

fn process_alive(pid: &str) -> bool {
    Command::new("kill")
        .args(["-0", pid])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .map(|s| s.success())
        .unwrap_or(false)
}

fn command_exists(name: &str) -> bool {
    Command::new("sh")
        .arg("-c")
        .arg(format!("command -v {name} >/dev/null 2>&1"))
        .status()
        .map(|s| s.success())
        .unwrap_or(false)
}

fn run_checked(cmd: &str, args: &[&str]) -> Result<()> {
    let status = Command::new(cmd).args(args).status()?;
    if status.success() {
        Ok(())
    } else {
        Err(format!("{cmd} {} failed with {status}", args.join(" ")).into())
    }
}

fn run_checked_owned(cmd: &str, args: &[String]) -> Result<()> {
    let status = Command::new(cmd).args(args).status()?;
    if status.success() {
        Ok(())
    } else {
        Err(format!("{cmd} {} failed with {status}", args.join(" ")).into())
    }
}

fn run_status(cmd: &str, args: &[&str]) -> Result<bool> {
    Ok(Command::new(cmd)
        .args(args)
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .map(|s| s.success())
        .unwrap_or(false))
}

fn command_output(cmd: &str, args: &[&str]) -> Result<String> {
    let out = Command::new(cmd).args(args).output()?;
    if !out.status.success() {
        return Err(format!("{cmd} {} failed", args.join(" ")).into());
    }
    Ok(String::from_utf8_lossy(&out.stdout).trim().to_string())
}

fn filesystem_type_for(path: &Path) -> Option<String> {
    command_output(
        "findmnt",
        &["-T", path_str(path).ok()?, "-n", "-o", "FSTYPE"],
    )
    .ok()
    .filter(|s| !s.is_empty())
}

fn phase_key(phase: &str) -> String {
    phase
        .chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() {
                c.to_ascii_uppercase()
            } else {
                '_'
            }
        })
        .collect()
}

fn path_str(path: &Path) -> Result<&str> {
    path.to_str()
        .ok_or_else(|| format!("path is not valid UTF-8: {}", path.display()).into())
}

fn json_escape(s: &str) -> String {
    s.replace('\\', "\\\\").replace('"', "\\\"")
}

fn du_h(path: &Path) -> String {
    command_output("du", &["-h", path_str(path).unwrap_or("")])
        .ok()
        .and_then(|out| out.split_whitespace().next().map(|s| s.to_string()))
        .unwrap_or_else(|| "-".to_string())
}

fn utc_timestamp() -> String {
    let secs = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    command_output(
        "date",
        &["-u", "-d", &format!("@{secs}"), "+%Y-%m-%dT%H:%M:%SZ"],
    )
    .unwrap_or_else(|_| secs.to_string())
}

fn unix_timestamp_secs() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn expand_cpu_list_accepts_ranges_and_singletons() {
        let got = expand_cpu_list("4-6,10,12-13").unwrap();
        assert_eq!(got, vec![4, 5, 6, 10, 12, 13]);
    }

    #[test]
    fn expand_cpu_list_rejects_descending_ranges() {
        assert!(expand_cpu_list("8-4").is_err());
    }

    #[test]
    fn format_cpu_list_compacts_adjacent_ranges() {
        let got = format_cpu_list(&[4, 5, 6, 10, 12, 13]);
        assert_eq!(got, "4-6,10,12-13");
    }

    #[test]
    fn calculate_cpu_placements_dedicates_when_capacity_is_available() {
        let placements = calculate_cpu_placements(
            &[
                CpuRequest {
                    id: "build-1".to_string(),
                    vcpus: 2,
                },
                CpuRequest {
                    id: "build-0".to_string(),
                    vcpus: 4,
                },
                CpuRequest {
                    id: "build-2".to_string(),
                    vcpus: 2,
                },
            ],
            &[4, 5, 6, 7, 8, 9, 10, 11],
        );

        let build_0 = placements.get("build-0").unwrap();
        let build_1 = placements.get("build-1").unwrap();
        let build_2 = placements.get("build-2").unwrap();
        assert_eq!(build_0.mode, CPU_PLACEMENT_DEDICATED);
        assert_eq!(build_1.mode, CPU_PLACEMENT_DEDICATED);
        assert_eq!(build_2.mode, CPU_PLACEMENT_DEDICATED);
        assert_eq!(build_0.host_cpus, vec![4, 5, 6, 7]);
        assert_eq!(build_1.host_cpus, vec![8, 9]);
        assert_eq!(build_2.host_cpus, vec![10, 11]);
    }

    #[test]
    fn calculate_cpu_placements_shares_and_wraps_when_overprovisioned() {
        let placements = calculate_cpu_placements(
            &[
                CpuRequest {
                    id: "build-0".to_string(),
                    vcpus: 4,
                },
                CpuRequest {
                    id: "build-1".to_string(),
                    vcpus: 4,
                },
                CpuRequest {
                    id: "build-2".to_string(),
                    vcpus: 4,
                },
            ],
            &[4, 5, 6, 7, 8, 9, 10, 11],
        );

        let build_0 = placements.get("build-0").unwrap();
        let build_1 = placements.get("build-1").unwrap();
        let build_2 = placements.get("build-2").unwrap();
        assert_eq!(build_0.mode, CPU_PLACEMENT_SHARED);
        assert_eq!(build_1.mode, CPU_PLACEMENT_SHARED);
        assert_eq!(build_2.mode, CPU_PLACEMENT_SHARED);
        assert_eq!(build_0.host_cpus, vec![4, 5, 6, 7]);
        assert_eq!(build_1.host_cpus, vec![8, 9, 10, 11]);
        assert_eq!(build_2.host_cpus, vec![4, 5, 6, 7]);
    }

    #[test]
    fn cpu_window_caps_at_allowed_count() {
        let got = cpu_window(&[4, 5], 0, 4);
        assert_eq!(got, vec![4, 5]);
    }

    #[test]
    fn tap_dev_for_id_keeps_interface_names_short_and_distinct() {
        let first = tap_dev_for_id("rust-auto-pin-1");
        let second = tap_dev_for_id("rust-auto-pin-2");

        assert_ne!(first, second);
        assert!(first.len() <= 15);
        assert!(second.len() <= 15);
        assert!(first.starts_with("fcrustaut"));
    }

    #[test]
    fn snapshot_rootfs_moves_consumed_instance_rootfs() {
        let dir = env::temp_dir().join(format!(
            "firecrackerctl-snapshot-test-{}",
            unix_timestamp_secs()
        ));
        fs::create_dir_all(&dir).unwrap();
        let source = dir.join("rootfs.ext4");
        let dest = dir.join("template.ext4");
        fs::write(&source, b"rootfs").unwrap();

        snapshot_rootfs(&source, &dest, true).unwrap();

        assert!(!source.exists());
        assert_eq!(fs::read(&dest).unwrap(), b"rootfs");
        fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn http_response_complete_handles_header_only_response() {
        let response = b"HTTP/1.1 204 No Content\r\nServer: firecracker\r\n\r\n";
        assert!(http_response_complete(response));
    }

    #[test]
    fn http_response_complete_waits_for_content_length_body() {
        let partial = b"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nok";
        let complete = b"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello";

        assert!(!http_response_complete(partial));
        assert!(http_response_complete(complete));
    }

    #[test]
    fn reserve_manual_cpu_placements_removes_manual_cpus() {
        let mut allowed = vec![4, 5, 6, 7, 8, 9];
        let claims = vec![CpuPlacementClaim {
            id: "manual".to_string(),
            vcpus: 2,
            host_cpus: Some("6-7".to_string()),
            cpu_placement_mode: Some(CPU_PLACEMENT_MANUAL.to_string()),
        }];

        reserve_manual_cpu_placements(&mut allowed, &claims).unwrap();

        assert_eq!(allowed, vec![4, 5, 8, 9]);
    }

    #[test]
    fn cpu_reservation_expiration_requires_fresh_timestamp() {
        let mut state = BTreeMap::new();
        assert!(cpu_reservation_expired(&state));

        let old = unix_timestamp_secs().saturating_sub(CPU_RESERVATION_TTL_SECS + 1);
        state.insert("CREATED_AT_EPOCH".to_string(), old.to_string());
        assert!(cpu_reservation_expired(&state));

        state.insert(
            "CREATED_AT_EPOCH".to_string(),
            unix_timestamp_secs().to_string(),
        );
        assert!(!cpu_reservation_expired(&state));
    }
}
