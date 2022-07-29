use std::ffi::CStr;
// use blake3_processing::creator;

#[no_mangle]
pub extern "C" fn init_stuff() {
    env_logger::init();

    log::trace!("init_stuff() trace level message");
    log::debug!("init_stuff() debug level message");
    log::info!("init_stuff() info level message");
    log::warn!("init_stuff() warn level message");
    log::error!("init_stuff() error level message");
}

#[no_mangle]
pub extern "C" fn process_file(file_name: *const libc::c_char) {
    let buf_name = unsafe { CStr::from_ptr(file_name).to_bytes() };
    let str_name = String::from_utf8(buf_name.to_vec()).unwrap();
    println!("Processing: {}", str_name);
    // touch the file
    let mut file = File::create(str_name).unwrap();
}

// This is present so it's easy to test that the code works natively in Rust via `cargo test
#[cfg(test)]
pub mod test {

    use std::ffi::CString;
    use super::*;

    // This is meant to do the same stuff as the main function in the .go files.
    #[test]
    fn simulated_main_function () {
        init_stuff();
        hello(CString::new("./ethereum.pdf").unwrap().into_raw());
    }

}
